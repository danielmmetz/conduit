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
	"syscall"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/danielmmetz/conduit/internal/xfer"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := mainE(ctx, logger, os.Stdin, os.Stdout, os.Args[1:]); err != nil {
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

func mainE(ctx context.Context, logger *slog.Logger, stdin io.Reader, out io.Writer, args []string) error {
	root := ffcli.Command{
		Name:       "conduit",
		ShortUsage: "conduit <send|recv> [flags] ...",
		ShortHelp:  "Share text or files between two devices over a rendezvous server.",
		Subcommands: []*ffcli.Command{
			sendCmd(logger, stdin, out),
			recvCmd(logger, out),
		},
		Exec: func(context.Context, []string) error { return flag.ErrHelp },
	}
	return root.ParseAndRun(ctx, args)
}

func sendCmd(logger *slog.Logger, stdin io.Reader, out io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit send", flag.ContinueOnError)
	var server, text string
	var noRelay, forceRelay bool
	fs.StringVar(&server, "server", "http://localhost:8080", "signaling server base URL")
	fs.StringVar(&text, "text", "", "text payload to send instead of a file")
	fs.BoolVar(&noRelay, "no-relay", false, "refuse TURN; fail rather than fall back to a relayed path")
	fs.BoolVar(&forceRelay, "force-relay", false, "force TURN; gather only relay candidates (useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send [--text <message> | <path>... | -] [--server URL]",
		ShortHelp:  "Reserve a slot and stream a payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			policy, err := client.RelayPolicyFromFlags(noRelay, forceRelay)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			src, err := openSource(text, args, stdin)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer src.Close()
			onProgress := func(total int64) {
				logger.Debug("peer acked bytes", slog.Int64("total", total))
			}
			if err := client.Send(ctx, logger, server, policy, src.Preamble, src.Reader, func(code string) {
				fmt.Fprintf(out, "code: %s\n", code)
				fmt.Fprintln(out, "waiting for receiver... (ctrl-c to cancel)")
			}, onProgress); err != nil {
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
	fs.StringVar(&outPath, "o", "", "write the received payload to this path ('-' for stdout; default is the sender's filename for files or the working directory for directories)")
	fs.BoolVar(&noRelay, "no-relay", false, "refuse TURN; fail rather than fall back to a relayed path")
	fs.BoolVar(&forceRelay, "force-relay", false, "force TURN; gather only relay candidates (useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [-o PATH | -] [--server URL]",
		ShortHelp:  "Join a slot and receive a payload from the sender.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			policy, err := client.RelayPolicyFromFlags(noRelay, forceRelay)
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			if len(args) < 1 {
				return fmt.Errorf("usage: conduit recv <code> [-]")
			}
			code, err := wire.ParseCode(args[0])
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			if len(code.Words) == 0 {
				return fmt.Errorf("parsing recv code: %q is missing the word portion", args[0])
			}
			// Positional "-" after the code is shorthand for "-o -".
			sinkOutPath := outPath
			if len(args) == 2 && args[1] == xfer.StdoutMarker {
				if sinkOutPath != "" && sinkOutPath != xfer.StdoutMarker {
					return fmt.Errorf("running recv: positional %q conflicts with -o %q", xfer.StdoutMarker, sinkOutPath)
				}
				sinkOutPath = xfer.StdoutMarker
			} else if len(args) > 1 {
				return fmt.Errorf("usage: conduit recv <code> [-]")
			}
			opts := xfer.SinkOptions{OutPath: sinkOutPath, Stdout: out}
			open := func(pre wire.Preamble) (io.WriteCloser, error) {
				return xfer.OpenSink(pre, opts)
			}
			if err := client.Recv(ctx, logger, server, code, policy, open); err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			return nil
		},
	}
}

// openSource resolves the payload Source from --text, positional paths, or "-"
// stdin. Exactly one route must apply; mixing --text with paths is an error.
func openSource(text string, args []string, stdin io.Reader) (*xfer.Source, error) {
	if text != "" && len(args) > 0 {
		return nil, fmt.Errorf("resolving send source: pass either --text or a path, not both")
	}
	if text != "" {
		return xfer.OpenText(text), nil
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("resolving send source: pass --text <message>, one or more paths, or '-' for stdin")
	}
	src, err := xfer.OpenPaths(args, stdin)
	if err != nil {
		return nil, fmt.Errorf("resolving send source: %w", err)
	}
	return src, nil
}
