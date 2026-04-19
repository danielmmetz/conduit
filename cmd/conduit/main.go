// conduit is the CLI client for the conduit rendezvous service.
//
// Phase 2 scope: send/recv stubs that relay a plaintext message through the
// signaling server. PAKE, encryption, and WebRTC land in later phases.
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
	"strconv"
	"strings"
	"syscall"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
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
	root := &ffcli.Command{
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
	fs.StringVar(&text, "text", "", "text payload to send (required in phase 2)")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send --text <message> [--server URL]",
		ShortHelp:  "Reserve a slot and send a text payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, _ []string) error {
			if text == "" {
				return fmt.Errorf("--text is required in phase 2")
			}
			return runSend(ctx, logger, out, server, text)
		},
	}
}

func recvCmd(logger *slog.Logger, out io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit recv", flag.ContinueOnError)
	var server string
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [--server URL]",
		ShortHelp:  "Join a slot and print the received text payload.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("usage: conduit recv <code>")
			}
			slot, err := parseCode(args[0])
			if err != nil {
				return fmt.Errorf("recv: %w", err)
			}
			return runRecv(ctx, logger, out, server, slot)
		},
	}
}

func runSend(ctx context.Context, logger *slog.Logger, out io.Writer, server, text string) error {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return fmt.Errorf("resolving ws url: %w", err)
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
		return fmt.Errorf("reserved handshake: %w", err)
	}
	logger.DebugContext(ctx, "slot reserved", slog.Uint64("slot", uint64(reserved.Slot)))
	fmt.Fprintf(out, "code: %d\n", reserved.Slot)
	fmt.Fprintln(out, "waiting for receiver... (ctrl-c to cancel)")

	paired, err := readCtl(ctx, conn)
	if err != nil {
		return fmt.Errorf("reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return fmt.Errorf("paired handshake: %w", err)
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(text)); err != nil {
		return fmt.Errorf("sending text: %w", err)
	}
	fmt.Fprintln(out, "sent")
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

func runRecv(ctx context.Context, logger *slog.Logger, out io.Writer, server string, slot uint32) error {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return fmt.Errorf("resolving ws url: %w", err)
	}

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer conn.CloseNow()

	if err := writeCtl(ctx, conn, wire.ClientHello{Op: wire.OpJoin, Slot: slot}); err != nil {
		return fmt.Errorf("sending join: %w", err)
	}

	paired, err := readCtl(ctx, conn)
	if err != nil {
		return fmt.Errorf("reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return fmt.Errorf("paired handshake: %w", err)
	}
	logger.DebugContext(ctx, "paired", slog.Uint64("slot", uint64(slot)))

	_, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading payload: %w", err)
	}
	fmt.Fprintln(out, string(data))
	_ = conn.Close(websocket.StatusNormalClosure, "")
	return nil
}

// parseCode accepts either a bare numeric slot ("42") or the full
// "42-word-word-word" form, keeping the numeric prefix. The wordlist portion
// is ignored until phase 3 adds PAKE.
func parseCode(code string) (uint32, error) {
	head, _, _ := strings.Cut(code, "-")
	n, err := strconv.ParseUint(head, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid code %q: %w", code, err)
	}
	return uint32(n), nil
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
		return fmt.Errorf("expected op %q, got %q", want, env.Op)
	}
	return nil
}
