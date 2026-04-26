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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/danielmmetz/conduit/internal/xfer"
	"github.com/peterbourgon/ff/v3"
	"github.com/peterbourgon/ff/v3/ffcli"
	"rsc.io/qr"
)

// defaultServerURL is the hosted rendezvous service. CLI defaults and the
// "receive on the CLI" hint both use it; the hint omits --server when the
// active URL matches, since the receiver's CLI default is the same.
const defaultServerURL = "https://conduit.danielmmetz.com"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := mainE(ctx, logger, os.Stdin, os.Stdout, os.Stderr, os.Args[1:]); err != nil {
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

func mainE(ctx context.Context, logger *slog.Logger, stdin io.Reader, out, stderr io.Writer, args []string) error {
	root := ffcli.Command{
		Name:       "conduit",
		ShortUsage: "conduit <send|recv> [flags] ...",
		ShortHelp:  "Share text or files between two devices over a rendezvous server.",
		Subcommands: []*ffcli.Command{
			sendCmd(logger, stdin, out, stderr),
			recvCmd(logger, out, stderr),
		},
		Exec: func(context.Context, []string) error { return flag.ErrHelp },
	}
	return root.ParseAndRun(ctx, args)
}

func sendCmd(logger *slog.Logger, stdin io.Reader, out, stderr io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit send", flag.ContinueOnError)
	var server, text string
	var showQR, git bool
	var policy client.RelayPolicy
	fs.StringVar(&server, "server", defaultServerURL, "signaling server base URL")
	fs.StringVar(&text, "text", "", "text payload to send instead of a file")
	fs.BoolVar(&showQR, "qr", false, "after printing the code, render a QR of the browser URL for scanning from a phone")
	fs.BoolVar(&git, "git", true, "for directory sends, honor a root .gitignore and skip .git/; --git=false sends the tree verbatim")
	fs.Var(&policy, "relay", "ICE relay policy: auto (default), never (refuse TURN; fail rather than fall back), or always (TURN-only, useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send [--text <message> | <path>... | -] [--server URL]",
		ShortHelp:  "Reserve a slot and stream a payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			src, err := openSource(text, args, stdin, git)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer src.Close()
			pr := newProgressLine(stderr, "↑")
			defer pr.Done()
			onCode := func(code string) {
				fmt.Fprintf(out, "code: %s\n", code)
				page, pageErr := receivePageURL(server, code)
				if pageErr == nil {
					fmt.Fprintf(out, "receive in the browser: %s\n", page)
				}
				fmt.Fprintf(out, "receive on the CLI: %s\n", recvCLIHint(server, code))
				if showQR && pageErr == nil {
					if err := renderQR(out, page); err != nil {
						fmt.Fprintf(stderr, "qr render failed: %v\n", err)
					}
				}
				fmt.Fprintln(out, "waiting for receiver... (ctrl-c to cancel)")
			}
			sess, err := client.OpenSender(ctx, logger, server, policy, onCode, nil)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer sess.Close(ctx)
			pr.Update(0, src.Preamble.Size)
			if err := sess.Push(ctx, src.Preamble, src.Reader); err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			pr.Update(src.Preamble.Size, src.Preamble.Size)
			pr.Finish(" ✓")
			return nil
		},
	}
}

func recvCmd(logger *slog.Logger, out, stderr io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit recv", flag.ContinueOnError)
	var server, outPath string
	var policy client.RelayPolicy
	fs.StringVar(&server, "server", defaultServerURL, "signaling server base URL")
	fs.StringVar(&outPath, "o", "", "write the received payload to this path ('-' for stdout; default is the sender's filename for files or the working directory for directories)")
	fs.Var(&policy, "relay", "ICE relay policy: auto (default), never (refuse TURN; fail rather than fall back), or always (TURN-only, useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [-o PATH | -] [--server URL]",
		ShortHelp:  "Join a slot and receive a payload from the sender.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
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
			pr := newProgressLine(stderr, "↓")
			defer pr.Done()
			done := make(chan error, 1)
			var writtenAnnounce string
			open := func(pre wire.Preamble) (io.WriteCloser, error) {
				sink, err := xfer.OpenSink(pre, opts)
				if err != nil {
					done <- err
					return nil, err
				}
				// Skip progress when the sink writes to stdout — the
				// progress line uses \r and would overwrite the payload
				// the user is trying to read. Disk-bound transfers still
				// get the progress meter and a final ✓.
				toStdout := sinkWritesToStdout(pre, sinkOutPath)
				var cb func(int64)
				if !toStdout {
					totalSize := pre.Size
					cb = func(received int64) { pr.Update(received, totalSize) }
					writtenAnnounce = sinkAnnounceName(pre, sinkOutPath)
				}
				return &finalizingSink{
					w:  sink,
					cb: cb,
					onClose: func() error {
						done <- nil
						return nil
					},
				}, nil
			}
			sess, err := client.OpenReceiver(ctx, logger, server, code, policy, open)
			if err != nil {
				return fmt.Errorf("running recv: %w", err)
			}
			defer sess.Close(ctx)
			select {
			case err := <-done:
				if err != nil {
					return fmt.Errorf("running recv: %w", err)
				}
				pr.Finish(" ✓")
				if writtenAnnounce != "" {
					fmt.Fprintf(out, "wrote %s\n", writtenAnnounce)
				}
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
}

// openSource resolves the payload Source from --text, positional paths, or "-"
// stdin. Exactly one route must apply; mixing --text with paths is an error.
func openSource(text string, args []string, stdin io.Reader, git bool) (*xfer.Source, error) {
	if text != "" && len(args) > 0 {
		return nil, fmt.Errorf("resolving send source: pass either --text or a path, not both")
	}
	if text != "" {
		return xfer.OpenText(text), nil
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("resolving send source: pass --text <message>, one or more paths, or '-' for stdin")
	}
	src, err := xfer.OpenPaths(args, stdin, git)
	if err != nil {
		return nil, fmt.Errorf("resolving send source: %w", err)
	}
	return src, nil
}

// sinkWritesToStdout reports whether xfer.OpenSink will route this
// preamble + outPath combination to stdout, mirroring the same logic in
// internal/xfer/sink.go. The recv command uses this to suppress the
// progress meter for cases where its carriage-return output would
// overwrite the payload the user is trying to read.
func sinkWritesToStdout(pre wire.Preamble, outPath string) bool {
	if outPath == xfer.StdoutMarker {
		return true
	}
	if outPath != "" {
		return false
	}
	if pre.Kind == wire.PreambleKindText {
		return true
	}
	if pre.Kind == wire.PreambleKindFile && (pre.Name == "" || pre.Name == "stdin") {
		return true
	}
	return false
}

// sinkAnnounceName returns a human-readable description of where a
// successfully-received payload was written, for printing after the
// transfer completes. Returns "" for stdout-bound sinks (the user already
// sees the payload). Mirrors xfer.OpenSink's path resolution so the
// announce stays consistent with where the bytes actually land.
func sinkAnnounceName(pre wire.Preamble, outPath string) string {
	if outPath != "" && outPath != xfer.StdoutMarker {
		return outPath
	}
	if pre.Kind == wire.PreambleKindFile && pre.Name != "" && pre.Name != "stdin" {
		return filepath.Base(pre.Name)
	}
	if pre.Kind == wire.PreambleKindTar {
		// openTarSink extracts to opts.OutPath or CWD when unset.
		if outPath != "" {
			return outPath + "/"
		}
		return "./"
	}
	return ""
}

// recvCLIHint is the "conduit recv ..." command shown to the sender. The
// --server flag is omitted when server matches the CLI's own default, so the
// hint stays short for the common hosted case.
func recvCLIHint(server, code string) string {
	if server == defaultServerURL {
		return fmt.Sprintf("conduit recv %s", code)
	}
	return fmt.Sprintf("conduit recv --server %s %s", server, code)
}

// receivePageURL is the web UI URL that pre-fills the code in the fragment (same host as --server).
func receivePageURL(server, code string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("parsing server %q: %w", server, err)
	}
	u.Fragment = code
	return u.String(), nil
}

// renderQR writes s to w as a QR using Unicode half-blocks (two QR rows per terminal row).
func renderQR(w io.Writer, s string) error {
	code, err := qr.Encode(s, qr.M)
	if err != nil {
		return fmt.Errorf("encoding qr: %w", err)
	}
	// 4-module quiet zone is part of the QR spec; without it many phone scanners refuse to lock.
	const quiet = 4
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bot := black(x, y), black(x, y+1)
			switch {
			case top && bot:
				b.WriteRune('█')
			case top:
				b.WriteRune('▀')
			case bot:
				b.WriteRune('▄')
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(w, b.String())
	return err
}

// progressLine renders single-line transfer progress to w using a carriage
// return so successive updates overwrite the previous line. Updates are
// throttled so a fast in-memory transfer doesn't flood the terminal.
type progressLine struct {
	w        io.Writer
	prefix   string
	mu       sync.Mutex
	last     time.Time
	maxWidth int
	any      bool
	done     bool
}

const progressMinInterval = 100 * time.Millisecond

func newProgressLine(w io.Writer, prefix string) *progressLine {
	return &progressLine{w: w, prefix: prefix}
}

func (p *progressLine) Update(done, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// Always emit when the transfer reaches its known total so the user sees a
	// final 100% line; the throttle would otherwise suppress an ack that lands
	// in the same window as the prior update.
	final := total > 0 && done >= total
	if !final && p.any && now.Sub(p.last) < progressMinInterval {
		return
	}
	p.last = now
	p.any = true
	p.write(p.formatLine(done, total))
}

// Done emits a trailing newline so subsequent output starts on a fresh
// line. Idempotent — repeated calls are no-ops, so callers can both defer
// Done for cleanup-on-error and call it explicitly to separate progress
// output from a follow-up status line on stdout.
func (p *progressLine) Done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done || !p.any {
		return
	}
	p.done = true
	_, _ = fmt.Fprint(p.w, "\n")
}

// Finish emits suffix (typically " ✓") at the end of the most recent
// progress line, then a newline. Idempotent. Used to mark the transfer
// complete in-place instead of letting the bare progress line dangle.
func (p *progressLine) Finish(suffix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done || !p.any {
		return
	}
	p.done = true
	_, _ = fmt.Fprint(p.w, suffix+"\n")
}

func (p *progressLine) formatLine(done, total int64) string {
	if total > 0 {
		pct := float64(done) / float64(total) * 100
		return fmt.Sprintf("%s %s / %s (%.0f%%)", p.prefix, humanBytes(done), humanBytes(total), pct)
	}
	return fmt.Sprintf("%s %s", p.prefix, humanBytes(done))
}

func (p *progressLine) write(line string) {
	if len(line) > p.maxWidth {
		p.maxWidth = len(line)
	}
	pad := p.maxWidth - len(line)
	padding := ""
	if pad > 0 {
		padding = fmt.Sprintf("%*s", pad, "")
	}
	_, _ = fmt.Fprintf(p.w, "\r%s%s", line, padding)
}

// humanBytes formats n as a binary-prefixed byte count using KB/MB/GB labels.
// Mirrors the formatting used by the browser UI so CLI and web read the same.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	for nn := n / unit; nn >= unit && exp < len(units)-1; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), units[exp])
}

// progressSink wraps an io.WriteCloser, invoking cb with the cumulative byte
// count after each successful Write so the receive command can render
// progressive progress without changing the rtc / client signatures.
type progressSink struct {
	w  io.WriteCloser
	cb func(int64)
	n  int64
}

func (p *progressSink) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.n += int64(n)
		if p.cb != nil {
			p.cb(p.n)
		}
	}
	return n, err
}

func (p *progressSink) Close() error { return p.w.Close() }

// finalizingSink wraps a writable sink with a progress callback and an
// onClose hook fired after the sink's own Close. The receive command uses
// this to learn when the first transfer's payload has been fully flushed
// to disk so it can return out of its Wait-for-first-transfer loop.
type finalizingSink struct {
	w       io.WriteCloser
	cb      func(int64)
	onClose func() error
	n       int64
}

func (p *finalizingSink) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.n += int64(n)
		if p.cb != nil {
			p.cb(p.n)
		}
	}
	return n, err
}

func (p *finalizingSink) Close() error {
	if err := p.w.Close(); err != nil {
		if p.onClose != nil {
			_ = p.onClose()
		}
		return err
	}
	if p.onClose != nil {
		return p.onClose()
	}
	return nil
}
