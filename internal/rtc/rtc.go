// Package rtc establishes a WebRTC data channel between two conduit peers and
// shuttles an age-encrypted payload over it.
//
// Signaling (SDP offer/answer) is exchanged over a message-oriented transport
// provided by the caller — typically the WebSocket already paired by the
// rendezvous server. The SDP bodies are themselves age-encrypted with the
// PAKE-derived session key K so the server and any TURN relay cannot observe
// ICE candidates or media lines.
//
// Callers choose direct vs. TURN-relayed transport by populating cfg.ICEServers
// and cfg.TransportPolicy. The default policy gathers host/srflx/relay
// candidates alongside each other, which lets pion prefer a direct path when
// one is reachable and fall back to the relay otherwise.
package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/webrtc/v4"
	"golang.org/x/sync/errgroup"
)

// Config controls ICE behavior and logging. ICEServers may be nil.
// TransportPolicy defaults to ICETransportPolicyAll; set to
// ICETransportPolicyRelay to force traffic through TURN (exercises the relay
// path; fails closed if no TURN server is reachable).
//
// OnRemoteProgress, if set on the sender, is invoked each time the receiver
// acknowledges additional plaintext bytes. The argument is the cumulative
// total reported by the peer. Acks are best-effort and may be dropped if the
// data channel tears down mid-transfer; callers must not treat them as
// integrity information.
type Config struct {
	ICEServers       []webrtc.ICEServer
	TransportPolicy  webrtc.ICETransportPolicy
	Logger           *slog.Logger
	OnRemoteProgress func(totalBytes int64)
}

type signalMsg struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// teardownSignal is exchanged bidirectionally on the signaling connection
// after the encrypted payload has been fully transferred at the application
// layer. Each peer sends teardownSignal once its own half of the transfer is
// complete, then waits for the peer's matching signal before closing the
// PeerConnection. This avoids relying on SCTP stream-reset semantics, which
// are not exposed uniformly across pion/native and pion/js DataChannel
// implementations.
const teardownSignal = "data_teardown"

// datachanWait records the first detached data channel or error from pion
// callbacks (whichever completes first). A later detached channel is closed
// if the race is lost so we do not leak the handle.
type datachanWait struct {
	once sync.Once
	done chan struct{}
	rwc  io.ReadWriteCloser
	err  error
}

func newDatachanWait() *datachanWait {
	return &datachanWait{done: make(chan struct{})}
}

func (d *datachanWait) setRWC(r io.ReadWriteCloser) {
	ran := false
	d.once.Do(func() {
		ran = true
		d.rwc = r
		d.err = nil
		close(d.done)
	})
	if !ran && r != nil {
		_ = r.Close()
	}
}

func (d *datachanWait) setErr(err error) {
	d.once.Do(func() {
		d.rwc = nil
		d.err = err
		close(d.done)
	})
}

// wait blocks until the channel is open, d records an error, or ctx is done.
func (d *datachanWait) wait(ctx context.Context) (io.ReadWriteCloser, error) {
	select {
	case <-d.done:
		if d.rwc != nil {
			return d.rwc, nil
		}
		return nil, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Send opens a data channel, sends the SDP offer over sig, and once the
// channel is open streams age-encrypted payload from src to the peer. It
// returns after the payload has been flushed and the bidirectional teardown
// handshake has completed over sig.
func Send(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config, src io.Reader) error {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	defer func() { _ = pc.Close() }()

	dc, err := pc.CreateDataChannel("conduit", nil)
	if err != nil {
		return fmt.Errorf("sending: creating data channel: %w", err)
	}
	openWait := newDatachanWait()
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			openWait.setErr(fmt.Errorf("detaching data channel: %w", err))
			return
		}
		openWait.setRWC(raw)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cfg.Logger.DebugContext(ctx, "peer connection state", slog.String("state", s.String()))
		if s == webrtc.PeerConnectionStateFailed {
			openWait.setErr(fmt.Errorf("peer connection failed"))
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("sending: creating offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("sending: setting local offer: %w", err)
	}
	if err := waitGather(ctx, pc); err != nil {
		return fmt.Errorf("sending: %w", err)
	}

	if err := sendSignal(ctx, sig, key, signalMsg{Type: "offer", SDP: pc.LocalDescription().SDP}); err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	answer, err := recvSignal(ctx, sig, key)
	if err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	if answer.Type != "answer" {
		return fmt.Errorf("sending: expected answer, got %q", answer.Type)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		return fmt.Errorf("sending: setting remote answer: %w", err)
	}

	raw, werr := openWait.wait(ctx)
	if werr != nil {
		if errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
			return fmt.Errorf("sending: waiting for open: %w", werr)
		}
		return fmt.Errorf("sending: %w", werr)
	}

	// Ack reader runs concurrently with the payload writer: the receiver
	// sends tagAck frames back on the same data channel, and we surface
	// them through cfg.OnRemoteProgress. The goroutine terminates when
	// raw.Close() unblocks its Read, which the deferred cleanup below
	// guarantees on every return path. Skip the goroutine entirely when the
	// caller did not supply a callback so we do not spin up unused work.
	var ackEG errgroup.Group
	defer func() {
		_ = raw.Close()
		_ = ackEG.Wait()
	}()
	if cfg.OnRemoteProgress != nil {
		ackEG.Go(func() error {
			err := readAcks(raw, cfg.OnRemoteProgress)
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				return nil
			}
			cfg.Logger.DebugContext(ctx, "ack reader exit", slog.String("err", err.Error()))
			return nil
		})
	}

	tw := newTagWriter(raw)
	wc, err := wire.Encrypt(tw, key)
	if err != nil {
		return fmt.Errorf("sending: starting encrypt: %w", err)
	}
	if _, err := io.Copy(wc, src); err != nil {
		_ = wc.Close()
		return fmt.Errorf("sending: copying payload: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("sending: finalizing encrypt: %w", err)
	}
	// Emits a single tagEOF sentinel message so the peer's tagReader
	// returns io.EOF to age without depending on SCTP stream-reset
	// semantics (which pion's JS data channel does not expose).
	if err := tw.Close(); err != nil {
		return fmt.Errorf("sending: finalizing framing: %w", err)
	}
	if err := exchangeTeardown(ctx, sig, key); err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	return nil
}

// Recv accepts a data channel from the peer, responds to the SDP offer, and
// writes decrypted payload to dst until the age stream ends, then performs
// the bidirectional teardown handshake over sig.
func Recv(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config, dst io.Writer) error {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	defer func() { _ = pc.Close() }()

	openWait := newDatachanWait()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			raw, err := dc.Detach()
			if err != nil {
				openWait.setErr(fmt.Errorf("detaching data channel: %w", err))
				return
			}
			openWait.setRWC(raw)
		})
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cfg.Logger.DebugContext(ctx, "peer connection state", slog.String("state", s.String()))
		if s == webrtc.PeerConnectionStateFailed {
			openWait.setErr(fmt.Errorf("peer connection failed"))
		}
	})

	offer, err := recvSignal(ctx, sig, key)
	if err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	if offer.Type != "offer" {
		return fmt.Errorf("receiving: expected offer, got %q", offer.Type)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return fmt.Errorf("receiving: setting remote offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("receiving: creating answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("receiving: setting local answer: %w", err)
	}
	if err := waitGather(ctx, pc); err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	if err := sendSignal(ctx, sig, key, signalMsg{Type: "answer", SDP: pc.LocalDescription().SDP}); err != nil {
		return fmt.Errorf("receiving: %w", err)
	}

	raw, werr := openWait.wait(ctx)
	if werr != nil {
		if errors.Is(werr, context.Canceled) || errors.Is(werr, context.DeadlineExceeded) {
			return fmt.Errorf("receiving: waiting for open: %w", werr)
		}
		return fmt.Errorf("receiving: %w", werr)
	}
	defer raw.Close()

	tr := newTagReader(raw)
	pr, err := wire.Decrypt(tr, key)
	if err != nil {
		return fmt.Errorf("receiving: starting decrypt: %w", err)
	}
	aw := newAckingWriter(dst, raw)
	if _, err := io.Copy(aw, pr); err != nil {
		return fmt.Errorf("receiving: copying payload: %w", err)
	}
	if err := aw.Flush(); err != nil {
		cfg.Logger.DebugContext(ctx, "flushing final ack", slog.String("err", err.Error()))
	}
	if err := exchangeTeardown(ctx, sig, key); err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	return nil
}

func newPeerConnection(cfg Config) (*webrtc.PeerConnection, error) {
	var se webrtc.SettingEngine
	se.DetachDataChannels()
	applyNativeSettings(&se)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers:         cfg.ICEServers,
		ICETransportPolicy: cfg.TransportPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("creating peer connection: %w", err)
	}
	return pc, nil
}

func waitGather(ctx context.Context, pc *webrtc.PeerConnection) error {
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("ice gather: %w", ctx.Err())
	}
}

func sendSignal(ctx context.Context, mc wire.MsgConn, key []byte, msg signalMsg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshaling signal: %w", err)
	}
	var buf bytes.Buffer
	wc, err := wire.Encrypt(&buf, key)
	if err != nil {
		return fmt.Errorf("encrypting signal: %w", err)
	}
	if _, err := wc.Write(data); err != nil {
		_ = wc.Close()
		return fmt.Errorf("encrypting signal: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("finalizing signal: %w", err)
	}
	if err := mc.Send(ctx, buf.Bytes()); err != nil {
		return fmt.Errorf("sending signal: %w", err)
	}
	return nil
}

func recvSignal(ctx context.Context, mc wire.MsgConn, key []byte) (signalMsg, error) {
	raw, err := mc.Recv(ctx)
	if err != nil {
		return signalMsg{}, fmt.Errorf("receiving signal: %w", err)
	}
	pr, err := wire.Decrypt(bytes.NewReader(raw), key)
	if err != nil {
		return signalMsg{}, fmt.Errorf("decrypting signal: %w", err)
	}
	data, err := io.ReadAll(pr)
	if err != nil {
		return signalMsg{}, fmt.Errorf("decrypting signal: %w", err)
	}
	var out signalMsg
	if err := json.Unmarshal(data, &out); err != nil {
		return signalMsg{}, fmt.Errorf("unmarshaling signal: %w", err)
	}
	return out, nil
}

// exchangeTeardown performs the two-way teardown handshake: both peers send
// teardownSignal concurrently (so neither deadlocks waiting for the other to
// send first) and each reads the peer's matching signal. Returns only after
// both halves complete — at which point all application payload has been
// transferred and both PeerConnections can safely close.
func exchangeTeardown(ctx context.Context, mc wire.MsgConn, key []byte) error {
	var (
		eg   errgroup.Group
		peer signalMsg
	)
	eg.Go(func() error { return sendSignal(ctx, mc, key, signalMsg{Type: teardownSignal}) })
	got, recvErr := recvSignal(ctx, mc, key)
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("exchanging teardown: %w", err)
	}
	if recvErr != nil {
		return fmt.Errorf("exchanging teardown: %w", recvErr)
	}
	peer = got
	if peer.Type != teardownSignal {
		return fmt.Errorf("exchanging teardown: expected %q, got %q", teardownSignal, peer.Type)
	}
	return nil
}
