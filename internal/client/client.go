// Package client implements conduit rendezvous + PAKE + WebRTC transfer, shared
// by the CLI and the browser WASM build.
package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/webrtc/v4"
)

// RelayPolicy selects how TURN / ICE relay is used. It satisfies flag.Value
// so callers can bind it to a CLI flag directly.
type RelayPolicy int

const (
	RelayAuto RelayPolicy = iota
	RelayNone
	RelayOnly
)

func (r RelayPolicy) String() string {
	switch r {
	case RelayNone:
		return "never"
	case RelayOnly:
		return "always"
	default:
		return "auto"
	}
}

func (r *RelayPolicy) Set(s string) error {
	switch s {
	case "auto":
		*r = RelayAuto
	case "never":
		*r = RelayNone
	case "always":
		*r = RelayOnly
	default:
		return fmt.Errorf("invalid relay policy %q (want auto, never, or always)", s)
	}
	return nil
}

func (r RelayPolicy) transportPolicy() webrtc.ICETransportPolicy {
	if r == RelayOnly {
		return webrtc.ICETransportPolicyRelay
	}
	return webrtc.ICETransportPolicyAll
}

// SinkOpener resolves the destination for an incoming payload once the
// receiver has decoded the preamble. The returned writer receives the
// plaintext payload bytes; Close is invoked after the bytes are consumed.
type SinkOpener func(wire.Preamble) (io.WriteCloser, error)

// preambleSink is a state-machine io.Writer: the first frame of bytes written
// to it is interpreted as a length-prefixed [wire.Preamble], after which the
// sink calls openSink and forwards remaining bytes to the writer it returned.
// Splitting the preamble across multiple Write calls (as happens when age's
// chunked output straddles the boundary) is handled by buffering the unread
// prefix until enough bytes have arrived.
type preambleSink struct {
	openSink SinkOpener

	lenBuf  [4]byte
	lenN    int    // bytes of length prefix consumed
	bodyLen uint32 // announced preamble length, once lenN == 4
	body    []byte // accumulated preamble bytes
	sink    io.WriteCloser
	preSeen bool
}

func (p *preambleSink) Write(b []byte) (int, error) {
	total := len(b)
	// Fill the 4-byte length prefix.
	if p.lenN < 4 {
		n := copy(p.lenBuf[p.lenN:], b)
		p.lenN += n
		b = b[n:]
		if p.lenN < 4 {
			return total, nil
		}
		p.bodyLen = binary.BigEndian.Uint32(p.lenBuf[:])
		if p.bodyLen > wire.MaxPreambleBody {
			return total, fmt.Errorf("reading preamble: announced %d bytes, max %d", p.bodyLen, wire.MaxPreambleBody)
		}
	}
	// Fill the JSON body.
	if !p.preSeen {
		need := int(p.bodyLen) - len(p.body)
		if need > 0 {
			take := min(len(b), need)
			p.body = append(p.body, b[:take]...)
			b = b[take:]
		}
		if len(p.body) < int(p.bodyLen) {
			return total, nil
		}
		pre, err := decodePreamble(p.body)
		if err != nil {
			return total, fmt.Errorf("reading preamble: %w", err)
		}
		sink, err := p.openSink(pre)
		if err != nil {
			return total, fmt.Errorf("opening sink: %w", err)
		}
		p.sink = sink
		p.preSeen = true
	}
	// Forward any bytes remaining after the preamble.
	if len(b) > 0 {
		if _, err := p.sink.Write(b); err != nil {
			return total, fmt.Errorf("writing payload to sink: %w", err)
		}
	}
	return total, nil
}

func (p *preambleSink) Close() error {
	if p.sink == nil {
		return nil
	}
	if err := p.sink.Close(); err != nil {
		return fmt.Errorf("closing sink: %w", err)
	}
	return nil
}

func decodePreamble(body []byte) (wire.Preamble, error) {
	var pre wire.Preamble
	if err := json.Unmarshal(body, &pre); err != nil {
		return wire.Preamble{}, fmt.Errorf("decoding preamble: %w", err)
	}
	if err := wire.ValidatePreamble(pre); err != nil {
		return wire.Preamble{}, fmt.Errorf("decoding preamble: %w", err)
	}
	return pre, nil
}

func iceServersFromEnvelope(env wire.Envelope) []webrtc.ICEServer {
	if env.TURN == nil || len(env.TURN.URIs) == 0 {
		return nil
	}
	return []webrtc.ICEServer{{
		URLs:       env.TURN.URIs,
		Username:   env.TURN.Username,
		Credential: env.TURN.Credential,
	}}
}

// wsMsgConn adapts a coder/websocket connection to wire.MsgConn so the PAKE
// handshake and WebRTC SDP exchange share the same relayed transport.
type wsMsgConn struct {
	conn *websocket.Conn
}

func (w wsMsgConn) Send(ctx context.Context, data []byte) error {
	if err := w.conn.Write(ctx, websocket.MessageBinary, data); err != nil {
		return fmt.Errorf("writing ws frame: %w", err)
	}
	return nil
}

func (w wsMsgConn) Recv(ctx context.Context) ([]byte, error) {
	_, data, err := w.conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading ws frame: %w", err)
	}
	return data, nil
}

func wsURLFor(server string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parsing server %q: %w", server, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	case "":
		return "", fmt.Errorf("server URL %q missing scheme", server)
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws"
	return u.String(), nil
}

func writeCtl(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling control frame: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("writing control frame: %w", err)
	}
	return nil
}

func readCtl(ctx context.Context, conn *websocket.Conn) (wire.Envelope, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return wire.Envelope{}, fmt.Errorf("reading control frame: %w", err)
	}
	var env wire.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return wire.Envelope{}, fmt.Errorf("decoding control frame: %w", err)
	}
	return env, nil
}

func expectOp(env wire.Envelope, want string) error {
	if env.Op == wire.OpError {
		msg := env.Code
		if env.Message != "" {
			msg = fmt.Sprintf("%s: %s", env.Code, env.Message)
		}
		return fmt.Errorf("server error: %s", msg)
	}
	if env.Op != want {
		return fmt.Errorf("validating control op: want %q, got %q", want, env.Op)
	}
	return nil
}
