// Package rtc establishes a WebRTC data channel between two conduit peers and
// shuttles an age-encrypted payload over it.
//
// Signaling (SDP offer/answer) is exchanged over a message-oriented transport
// provided by the caller — typically the WebSocket already paired by the
// rendezvous server. The SDP bodies are themselves age-encrypted with the
// PAKE-derived session key K so the server and any TURN relay cannot observe
// ICE candidates or media lines.
//
// Phase 4 scope: direct path preferred; TURN URIs/creds from signaling are
// passed via cfg.ICEServers. Relay paths are validated in phase 5.
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
	"time"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// Config controls ICE behavior and logging. ICEServers may be nil.
type Config struct {
	ICEServers []webrtc.ICEServer
	Logger     *slog.Logger
}

type signalMsg struct {
	Type string `json:"type"`
	SDP  string `json:"sdp"`
}

// teardownSignal is sent on the signaling connection after SCTP data-channel
// teardown handshakes complete; the receiver waits for it before closing its
// PeerConnection so an SCTP ABORT does not overtake the outbound stream reset
// (Close queues the reset but does not wait for it to be transmitted).
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
// returns after the payload has been flushed, SCTP teardown on the data
// channel has completed, and a final teardown message has been sent on sig.
func Send(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config, src io.Reader) error {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	defer func() { _ = pc.GracefulClose() }()

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
	defer raw.Close()

	wc, err := wire.Encrypt(raw, key)
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
	// Signal end-of-stream to the peer via SCTP stream reset, so age's
	// decrypt sees EOF after the final chunk. Then block until we observe
	// the remote's matching reset (io.EOF on Read); this guarantees the
	// receiver drained before we tear down the PeerConnection.
	if err := raw.Close(); err != nil {
		return fmt.Errorf("sending: closing data channel: %w", err)
	}
	if err := waitPeerClose(ctx, raw); err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	if err := sendSignal(ctx, sig, key, signalMsg{Type: teardownSignal}); err != nil {
		return fmt.Errorf("sending: %w", err)
	}
	return nil
}

// Recv accepts a data channel from the peer, responds to the SDP offer, and
// writes decrypted payload to dst until the peer closes the channel, then
// reads one final teardown message from sig before closing the PeerConnection.
func Recv(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config, dst io.Writer) error {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	defer func() { _ = pc.GracefulClose() }()

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

	pr, err := wire.Decrypt(raw, key)
	if err != nil {
		return fmt.Errorf("receiving: starting decrypt: %w", err)
	}
	if _, err := io.Copy(dst, pr); err != nil {
		return fmt.Errorf("receiving: copying payload: %w", err)
	}
	// Reset our outgoing SCTP stream; the sender's Read observes EOF and
	// finishes waitPeerClose, then sends teardownSignal on the signaling
	// connection. We wait for that message before closing our PeerConnection
	// so an SCTP ABORT does not overtake our outbound stream reset.
	if err := raw.Close(); err != nil {
		return fmt.Errorf("receiving: closing data channel: %w", err)
	}
	last, err := recvSignal(ctx, sig, key)
	if err != nil {
		return fmt.Errorf("receiving: %w", err)
	}
	if last.Type != teardownSignal {
		return fmt.Errorf("receiving: expected teardown signal %q, got %q", teardownSignal, last.Type)
	}
	return nil
}

func newPeerConnection(cfg Config) (*webrtc.PeerConnection, error) {
	var se webrtc.SettingEngine
	se.DetachDataChannels()
	// Disable mDNS ICE candidate obfuscation. The default mode advertises
	// host candidates as random .local names resolvable only via an mDNS
	// responder, which breaks loopback connectivity in test environments
	// and constrained networks.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Without loopback candidates, same-host peers only see LAN IPs; under
	// load ICE can sit behind srflx/prflx minimum-wait timers and miss tight
	// CLI deadlines. Loopback host candidates make 127.0.0.1/::1 pairs
	// available immediately for local transfers.
	se.SetIncludeLoopbackCandidate(true)
	// Defaults wait 500ms/1s before nominating srflx/prflx pairs; that often
	// consumes the tail of short contexts even when a host pair is viable.
	se.SetSrflxAcceptanceMinWait(0)
	se.SetPrflxAcceptanceMinWait(0)
	// Default 5s STUN gather timeout can dominate small operation budgets when
	// srflx candidates are requested alongside sluggish STUN.
	se.SetSTUNGatherTimeout(time.Second)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: cfg.ICEServers})
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

// waitPeerClose blocks until rc observes io.EOF, meaning the peer has reset
// its outgoing SCTP stream. If ctx is canceled, it closes rc to unblock the
// inner Read, joins the watcher goroutine, and returns ctx.Err().
func waitPeerClose(ctx context.Context, rc io.ReadCloser) error {
	var wg sync.WaitGroup
	watchDone := make(chan struct{})
	wg.Go(func() {
		select {
		case <-ctx.Done():
			_ = rc.Close()
		case <-watchDone:
		}
	})
	_, err := io.Copy(io.Discard, rc)
	close(watchDone)
	wg.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("waiting for peer close: %w", ctx.Err())
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("waiting for peer close: %w", err)
	}
	return nil
}
