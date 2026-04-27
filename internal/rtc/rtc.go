// Package rtc establishes a WebRTC data channel between two conduit peers and
// shuttles an age-encrypted payload over it.
//
// Signaling — SDP offer/answer, trickled ICE candidates, end-of-candidates,
// and the bidirectional teardown handshake — flows over a message-oriented
// transport provided by the caller, typically the WebSocket already paired
// by the rendezvous server. Each frame is age-encrypted with the
// PAKE-derived session key K so the server and any TURN relay cannot
// observe ICE candidates or media lines. Trickle ICE means the SDP is sent
// immediately after SetLocalDescription, with no inline candidates;
// candidates flow as separate frames as pion gathers them, so cross-NAT
// pairings converge in seconds rather than tens of seconds.
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

// signalMsg is the JSON-encoded envelope exchanged over sig. Type
// disambiguates which fields are populated:
//
//   - "offer" / "answer": SDP only.
//   - "ice-candidate": Candidate + the optional sdpMid / sdpMLineIndex /
//     usernameFragment fields, mirroring webrtc.ICECandidateInit.
//   - "end-of-candidates": no payload — signals that the peer has gathered
//     all candidates so the local agent can finalize its remote set.
//   - teardownSignal: no payload — the bidirectional shutdown handshake.
type signalMsg struct {
	Type             string  `json:"type"`
	SDP              string  `json:"sdp,omitempty"`
	Candidate        string  `json:"candidate,omitempty"`
	SDPMid           *string `json:"sdpMid,omitempty"`
	SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
	UsernameFragment *string `json:"usernameFragment,omitempty"`
}

// isAnyOf reports whether err matches any of targets via errors.Is. Returns
// false for a nil err.
func isAnyOf(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
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

	// Register OnICECandidate before SetLocalDescription — pion can fire
	// candidates from inside SetLocalDescription, before that call returns.
	tm := newTrickleManager(ctx, pc, sig, key, cfg.Logger)
	defer tm.close()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return fmt.Errorf("sending: creating offer: %w", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		return fmt.Errorf("sending: setting local offer: %w", err)
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
	// Remote description is set; flush queued local candidates and start
	// consuming peer candidates from sig.
	tm.start(ctx)

	raw, werr := openWait.wait(ctx)
	if werr != nil {
		if isAnyOf(werr, context.Canceled, context.DeadlineExceeded) {
			return fmt.Errorf("sending: waiting for open: %w", werr)
		}
		return fmt.Errorf("sending: %w", werr)
	}
	// pion/wasm's detached data channel panics on a second Close (the native
	// variant is idempotent). Both this defer and readAcks's ctx watcher will
	// try to close raw, so funnel them through OnceValue.
	closeRaw := sync.OnceValue(raw.Close)

	// Ack reader runs concurrently with the payload writer: the receiver
	// sends tagAck frames back on the same data channel, and we surface
	// them through cfg.OnRemoteProgress. readAcks is ctx-aware (it spawns
	// its own watcher that calls closeRaw on ctx.Done), so the goroutine is
	// guaranteed to exit on either a clean peer close or ctx cancellation —
	// which lets the caller synchronously ackEG.Wait() below.
	var ackEG errgroup.Group
	defer func() {
		_ = closeRaw()
		_ = ackEG.Wait()
	}()
	if cfg.OnRemoteProgress != nil {
		ackEG.Go(func() error {
			err := readAcks(ctx, raw, func() { _ = closeRaw() }, cfg.OnRemoteProgress)
			if err != nil && !isAnyOf(err, io.EOF, io.ErrClosedPipe, context.Canceled, context.DeadlineExceeded) {
				cfg.Logger.DebugContext(ctx, "ack reader exit", slog.String("err", err.Error()))
			}
			return nil
		})
	}

	// On JS the underlying RTCDataChannel.send throws once bufferedAmount
	// crosses an undocumented browser ceiling; wrapSendWriter applies
	// bufferedAmount-based backpressure before each frame. Native pion
	// applies flow control in its SCTP layer, so the wrapper is a no-op.
	tw := newTagWriter(wrapSendWriter(dc, raw))
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
	// Block teardown until the ack reader has fully drained. readAcks is
	// ctx-aware, so this Wait is bounded by ctx — no select/bridge needed.
	// The barrier matters: the deferred raw.Close must not fire while the
	// reader is still dequeuing buffered frames from pion, or we drop the
	// final ack.
	_ = ackEG.Wait()
	if err := tm.exchangeTeardown(ctx); err != nil {
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

	// Register OnICECandidate before SetLocalDescription — pion can fire
	// candidates from inside SetLocalDescription, before that call returns.
	tm := newTrickleManager(ctx, pc, sig, key, cfg.Logger)
	defer tm.close()

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
	if err := sendSignal(ctx, sig, key, signalMsg{Type: "answer", SDP: pc.LocalDescription().SDP}); err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	// Remote description is set and our SDP has been sent; flush queued
	// local candidates and start consuming peer candidates from sig.
	tm.start(ctx)

	raw, werr := openWait.wait(ctx)
	if werr != nil {
		if isAnyOf(werr, context.Canceled, context.DeadlineExceeded) {
			return fmt.Errorf("receiving: waiting for open: %w", werr)
		}
		return fmt.Errorf("receiving: %w", werr)
	}
	// pion's wasm detachedDataChannel.Close closes an internal channel and
	// panics on a second call; the native variant is idempotent. Funnel both
	// the happy-path close and the error-path defer through sync.OnceValue so
	// we stay portable across build tags.
	closeRaw := sync.OnceValue(raw.Close)
	defer func() { _ = closeRaw() }()

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
	// Close the data channel before signaling teardown so the peer's ack
	// reader drains all pending ack frames via SCTP EOF. Closing after
	// teardown races with the sender's deferred local close and can drop the
	// final ack.
	if err := closeRaw(); err != nil {
		cfg.Logger.DebugContext(ctx, "closing data channel", slog.String("err", err.Error()))
	}
	if err := tm.exchangeTeardown(ctx); err != nil {
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

