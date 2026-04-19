// conduit is the CLI client for the conduit rendezvous service. After slot
// rendezvous over WebSocket, peers run SPAKE2 to derive a session key K, then
// exchange bytes over a WebRTC data channel. K is used both to age-encrypt SDP
// signaling relayed through the server and to age-encrypt the payload itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/danielmmetz/conduit/internal/client"
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
	var noRelay, forceRelay bool
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	fs.StringVar(&text, "text", "", "text payload to send instead of a file")
	fs.BoolVar(&noRelay, "no-relay", false, "refuse TURN; fail rather than fall back to a relayed path")
	fs.BoolVar(&forceRelay, "force-relay", false, "force TURN; gather only relay candidates (useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send [--text <message> | <path>] [--server URL]",
		ShortHelp:  "Reserve a slot and stream a payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			policy, err := client.RelayPolicyFromFlags(noRelay, forceRelay)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			src, err := openSource(text, args)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer src.Close()
			if err := client.Send(ctx, logger, server, policy, src, func(code string) {
				fmt.Fprintf(out, "code: %s\n", code)
				fmt.Fprintln(out, "waiting for receiver... (ctrl-c to cancel)")
			}); err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			fmt.Fprintln(out, "sent")
			return nil
		},
	}
}

func recvCmd(logger *slog.Logger, out io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit recv", flag.ContinueOnError)
	var server, outPath string
	var noRelay, forceRelay bool
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	fs.StringVar(&outPath, "o", "", "write the received payload to this path (default: stdout)")
	fs.BoolVar(&noRelay, "no-relay", false, "refuse TURN; fail rather than fall back to a relayed path")
	fs.BoolVar(&forceRelay, "force-relay", false, "force TURN; gather only relay candidates (useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [-o PATH] [--server URL]",
		ShortHelp:  "Join a slot and receive a payload from the sender.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			policy, err := client.RelayPolicyFromFlags(noRelay, forceRelay)
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
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
			if err := client.Recv(ctx, logger, server, code, policy, dst); err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			return nil
		},
	}
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
