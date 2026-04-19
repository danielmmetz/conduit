// Package client implements conduit rendezvous + PAKE + WebRTC transfer, shared
// by the CLI and the browser WASM build.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/rtc"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/webrtc/v4"
)

// RelayPolicy selects how TURN / ICE relay is used.
type RelayPolicy int

const (
	RelayAuto RelayPolicy = iota
	RelayNone
	RelayOnly
)

// RelayPolicyFromFlags maps CLI flags to a policy (--no-relay and --force-relay
// are mutually exclusive).
func RelayPolicyFromFlags(noRelay, forceRelay bool) (RelayPolicy, error) {
	switch {
	case noRelay && forceRelay:
		return RelayAuto, fmt.Errorf("choosing relay policy: --no-relay and --force-relay are mutually exclusive")
	case noRelay:
		return RelayNone, nil
	case forceRelay:
		return RelayOnly, nil
	default:
		return RelayAuto, nil
	}
}

func (r RelayPolicy) transportPolicy() webrtc.ICETransportPolicy {
	if r == RelayOnly {
		return webrtc.ICETransportPolicyRelay
	}
	return webrtc.ICETransportPolicyAll
}

// Send reserves a slot, invokes onCode with the human-readable code (before
// waiting for the peer), completes PAKE + WebRTC, and streams src to the receiver.
func Send(ctx context.Context, logger *slog.Logger, server string, policy RelayPolicy, src io.Reader, onCode func(code string)) error {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return fmt.Errorf("resolving websocket URL: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer conn.CloseNow()

	if err := writeCtl(ctx, conn, wire.ClientHello{Op: wire.OpReserve}); err != nil {
		return fmt.Errorf("sending reserve: %w", err)
	}

	reserved, err := readCtl(ctx, conn)
	if err != nil {
		return fmt.Errorf("reading reserved: %w", err)
	}
	if err := expectOp(reserved, wire.OpReserved); err != nil {
		return fmt.Errorf("handling reserved frame: %w", err)
	}
	logger.DebugContext(ctx, "slot reserved", slog.Uint64("slot", uint64(reserved.Slot)))

	code, err := wire.FormatCode(reserved.Slot)
	if err != nil {
		return fmt.Errorf("formatting code: %w", err)
	}
	if onCode != nil {
		onCode(code)
	}

	parsed, err := wire.ParseCode(code)
	if err != nil {
		return fmt.Errorf("parsing formatted code: %w", err)
	}

	paired, err := readCtl(ctx, conn)
	if err != nil {
		return fmt.Errorf("reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return fmt.Errorf("handling paired frame: %w", err)
	}

	key, err := wire.SendHandshakeMsg(ctx, wsMsgConn{conn: conn}, parsed)
	if err != nil {
		return fmt.Errorf("running pake handshake: %w", err)
	}
	logger.DebugContext(ctx, "pake key derived")

	ice := iceServersForSend(paired, reserved)
	if policy == RelayNone {
		ice = nil
	}
	cfg := rtc.Config{
		ICEServers:      ice,
		TransportPolicy: policy.transportPolicy(),
		Logger:          logger,
	}
	if err := rtc.Send(ctx, wsMsgConn{conn: conn}, key, cfg, src); err != nil {
		return fmt.Errorf("sending payload: %w", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

// Recv joins a slot, runs PAKE + WebRTC, and writes the decrypted payload to dst.
func Recv(ctx context.Context, logger *slog.Logger, server string, code wire.Code, policy RelayPolicy, dst io.Writer) error {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return fmt.Errorf("resolving websocket URL: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer conn.CloseNow()

	if err := writeCtl(ctx, conn, wire.ClientHello{Op: wire.OpJoin, Slot: code.Slot}); err != nil {
		return fmt.Errorf("sending join: %w", err)
	}

	paired, err := readCtl(ctx, conn)
	if err != nil {
		return fmt.Errorf("reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return fmt.Errorf("handling paired frame: %w", err)
	}
	logger.DebugContext(ctx, "paired", slog.Uint64("slot", uint64(code.Slot)))

	key, err := wire.RecvHandshakeMsg(ctx, wsMsgConn{conn: conn}, code)
	if err != nil {
		return fmt.Errorf("running pake handshake: %w", err)
	}
	logger.DebugContext(ctx, "pake key derived")

	ice := iceServersFromEnvelope(paired)
	if policy == RelayNone {
		ice = nil
	}
	cfg := rtc.Config{
		ICEServers:      ice,
		TransportPolicy: policy.transportPolicy(),
		Logger:          logger,
	}
	if err := rtc.Recv(ctx, wsMsgConn{conn: conn}, key, cfg, dst); err != nil {
		return fmt.Errorf("receiving payload: %w", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
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

func iceServersForSend(paired, reserved wire.Envelope) []webrtc.ICEServer {
	if s := iceServersFromEnvelope(paired); len(s) > 0 {
		return s
	}
	return iceServersFromEnvelope(reserved)
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
