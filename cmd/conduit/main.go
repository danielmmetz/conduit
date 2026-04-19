// conduit is the CLI client for the conduit rendezvous service. After slot
// rendezvous over WebSocket, peers run SPAKE2 to derive a session key K, then
// exchange bytes over a WebRTC data channel. K is used both to age-encrypt SDP
// signaling relayed through the server and to age-encrypt the payload itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/rtc"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"github.com/pion/webrtc/v4"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := mainE(ctx, logger, os.Stdout, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) || errors.Is(err, context.Canceled) {
			if ctx.Err() == nil {
				os.Exit(2)
			}
			return
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		if ctx.Err() == nil {
			os.Exit(1)
		}
	}
}

func mainE(ctx context.Context, logger *slog.Logger, out io.Writer, args []string) error {
	root := ffcli.Command{
		Name:       "conduit",
		ShortUsage: "conduit <send|recv> [flags] ...",
		ShortHelp:  "Share text or files between two devices over a rendezvous server.",
		Subcommands: []*ffcli.Command{
			sendCmd(logger, out),
			recvCmd(logger, out),
		},
		Exec: func(context.Context, []string) error { return flag.ErrHelp },
	}
	return root.ParseAndRun(ctx, args)
}

func sendCmd(logger *slog.Logger, out io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit send", flag.ContinueOnError)
	var server, text string
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	fs.StringVar(&text, "text", "", "text payload to send instead of a file")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send [--text <message> | <path>] [--server URL]",
		ShortHelp:  "Reserve a slot and stream a payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			src, err := openSource(text, args)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer src.Close()
			return runSend(ctx, logger, out, server, src)
		},
	}
}

func recvCmd(logger *slog.Logger, out io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit recv", flag.ContinueOnError)
	var server, outPath string
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	fs.StringVar(&outPath, "o", "", "write the received payload to this path (default: stdout)")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [-o PATH] [--server URL]",
		ShortHelp:  "Join a slot and receive a payload from the sender.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: conduit recv <code>")
			}
			code, err := wire.ParseCode(args[0])
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			if len(code.Words) == 0 {
				return fmt.Errorf("parsing recv code: %q is missing the word portion", args[0])
			}
			dst, err := openSink(outPath, out)
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			defer dst.Close()
			return runRecv(ctx, logger, server, code, dst)
		},
	}
}

func runSend(ctx context.Context, logger *slog.Logger, out io.Writer, server string, src io.ReadCloser) error {
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
	parsed, err := wire.ParseCode(code)
	if err != nil {
		return fmt.Errorf("parsing formatted code: %w", err)
	}
	fmt.Fprintf(out, "code: %s\n", code)
	fmt.Fprintln(out, "waiting for receiver... (ctrl-c to cancel)")

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

	cfg := rtc.Config{
		ICEServers: iceServersForSend(paired, reserved),
		Logger:     logger,
	}
	if err := rtc.Send(ctx, wsMsgConn{conn: conn}, key, cfg, src); err != nil {
		return fmt.Errorf("sending payload: %w", err)
	}
	fmt.Fprintln(out, "sent")
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func runRecv(ctx context.Context, logger *slog.Logger, server string, code wire.Code, dst io.WriteCloser) error {
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

	cfg := rtc.Config{
		ICEServers: iceServersFromEnvelope(paired),
		Logger:     logger,
	}
	if err := rtc.Recv(ctx, wsMsgConn{conn: conn}, key, cfg, dst); err != nil {
		return fmt.Errorf("receiving payload: %w", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

// openSource resolves the payload reader from --text or a positional path arg.
// Exactly one must be provided. On success, the result is always an
// io.ReadCloser (io.NopCloser for in-memory text; *os.File for paths).
func openSource(text string, args []string) (io.ReadCloser, error) {
	switch {
	case text != "" && len(args) > 0:
		return nil, fmt.Errorf("resolving send source: pass either --text or a path, not both")
	case text != "":
		return io.NopCloser(strings.NewReader(text)), nil
	case len(args) == 1:
		f, err := os.Open(args[0])
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", args[0], err)
		}
		return f, nil
	case len(args) == 0:
		return nil, fmt.Errorf("resolving send source: pass --text <message> or a path")
	default:
		return nil, fmt.Errorf("resolving send source: expected exactly one path, got %d", len(args))
	}
}

// writeSink is an io.WriteCloser. The stdlib provides io.NopCloser for readers
// but not an equivalent for arbitrary io.Writers (stdout, test doubles), so we
// use an optional close hook: nil means Close is a no-op.
type writeSink struct {
	w     io.Writer
	close func() error
}

func (s writeSink) Write(p []byte) (int, error) { return s.w.Write(p) }

func (s writeSink) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

// openSink resolves the payload sink from -o or falls back to fallback.
// On success, the result is always an io.WriteCloser.
func openSink(path string, fallback io.Writer) (io.WriteCloser, error) {
	if path == "" {
		return writeSink{w: fallback}, nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return writeSink{w: f, close: f.Close}, nil
}

// iceServersFromEnvelope maps TURN credentials in a control frame to pion ICE
// servers. Returns nil when none are present.
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

// iceServersForSend prefers paired (fresh at join time); falls back to reserved
// for older servers that only issued TURN on "reserved".
func iceServersForSend(paired, reserved wire.Envelope) []webrtc.ICEServer {
	if s := iceServersFromEnvelope(paired); len(s) > 0 {
		return s
	}
	return iceServersFromEnvelope(reserved)
}

// wsMsgConn adapts a coder/websocket connection to wire.MsgConn so the PAKE
// handshake and WebRTC SDP exchange can share the same relayed transport.
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
