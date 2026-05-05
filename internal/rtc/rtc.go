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

