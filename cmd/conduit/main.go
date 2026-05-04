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
	"sync/atomic"
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
	var server, text, compressFlag string
	var showQR, git bool
	var policy client.RelayPolicy
	fs.StringVar(&server, "server", defaultServerURL, "signaling server base URL")
	fs.StringVar(&text, "text", "", "text payload to send instead of a file")
	fs.BoolVar(&showQR, "qr", false, "after printing the code, render a QR of the browser URL for scanning from a phone")
	fs.BoolVar(&git, "git", true, "for directory sends, honor a root .gitignore and skip .git/; --git=false sends the tree verbatim")
	fs.StringVar(&compressFlag, "compress", "auto", "payload compression: auto (zstd unless the MIME is already compressed), zstd (force on), none (force off)")
	fs.Var(&policy, "relay", "ICE relay policy: auto (default), never (refuse TURN; fail rather than fall back), or always (TURN-only, useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "send",
		ShortUsage: "conduit send [--text <message> | <path>... | -] [--server URL]",
		ShortHelp:  "Reserve a slot and stream a payload to the peer that joins it.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			mode, err := parseCompressMode(compressFlag)
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			pr := newProgressLine(stderr, "↑")
			defer pr.Done()
			// Progress is driven by uncompressed bytes read from the user's
			// source — installed by xfer inside finalizeSource upstream of
			// the encoder. The total comes from Preamble.Size (user-visible
			// bytes; -1 for streaming tar / stdin → byte counter only).
			// total is atomic because the encoder goroutine inside finalizeSource
			// can begin reading (and firing the callback) before openSource
			// returns and we learn Preamble.Size on the main goroutine.
			var total atomic.Int64
			progress := func(n int64) { pr.Update(n, total.Load()) }
			src, err := openSource(text, args, stdin, git, xfer.SourceOptions{
				Compression: mode,
				Progress:    progress,
			})
			if err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			defer src.Close()
			total.Store(src.Preamble.Size)
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
			fmt.Fprintf(stderr, "connection: %s\n", sess.Route())
			totalSize := total.Load()
			pr.Update(0, totalSize)
			if err := sess.Push(ctx, src.Preamble, src.Reader); err != nil {
				return fmt.Errorf("running send: %w", err)
			}
			if totalSize > 0 {
				pr.Update(totalSize, totalSize)
			}
			pr.Finish(" ✓")
			return nil
		},
	}
}

// parseCompressMode maps the --compress flag string to xfer.CompressMode.
func parseCompressMode(s string) (xfer.CompressMode, error) {
	switch strings.ToLower(s) {
	case "auto":
		return xfer.CompressAuto, nil
	case "zstd":
		return xfer.CompressZstd, nil
	case "none", "off":
		return xfer.CompressNone, nil
	default:
		return 0, fmt.Errorf("invalid --compress %q (want auto, zstd, or none)", s)
	}
}

func recvCmd(logger *slog.Logger, out, stderr io.Writer) *ffcli.Command {
	fs := flag.NewFlagSet("conduit recv", flag.ContinueOnError)
	var server, outPath string
	var watch, showQR bool
	var policy client.RelayPolicy
	fs.StringVar(&server, "server", defaultServerURL, "signaling server base URL")
	fs.StringVar(&outPath, "o", "", "write the received payload to this path ('-' for stdout; default is the sender's filename for files or the working directory for directories)")
	fs.BoolVar(&watch, "watch", false, "stay open across multiple transfers; without a code the receiver hosts a stable persistent slot, with a code it joins and stays paired through the peer's session (-o must be empty or a directory in either mode)")
	fs.BoolVar(&showQR, "qr", false, "in --watch host mode, render a QR of the browser URL for scanning from a phone after printing the code")
	fs.Var(&policy, "relay", "ICE relay policy: auto (default), never (refuse TURN; fail rather than fall back), or always (TURN-only, useful for exercising the relay)")
	return &ffcli.Command{
		Name:       "recv",
		ShortUsage: "conduit recv <code> [-o PATH | -] [--server URL]\n       conduit recv --watch [<code>] [-o DIR] [--server URL]",
		ShortHelp:  "Receive a payload (one-shot), or with --watch keep the connection open: hosts a stable code with no positional, joins through one with a code.",
		FlagSet:    fs,
		Options:    []ff.Option{ff.WithEnvVarPrefix("CONDUIT")},
		Exec: func(ctx context.Context, args []string) error {
			if watch {
				return runRecvWatch(ctx, logger, server, policy, outPath, showQR, args, out, stderr)
			}
			return runRecvOnce(ctx, logger, server, policy, outPath, args, out, stderr)
		},
	}
}

// runRecvOnce is the original one-shot recv flow: join the supplied slot,
// receive one payload, exit.
func runRecvOnce(ctx context.Context, logger *slog.Logger, server string, policy client.RelayPolicy, outPath string, args []string, out, stderr io.Writer) error {
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
	pr := newProgressLine(stderr, "↓")
	defer pr.Done()
	done := make(chan error, 1)
	var writtenAnnounce string
	open := func(pre wire.Preamble) (io.WriteCloser, error) {
		// Skip progress when the sink writes to stdout — the progress
		// line uses \r and would overwrite the payload the user is
		// trying to read. Disk-bound transfers still get the meter.
		toStdout := sinkWritesToStdout(pre, sinkOutPath)
		opts := xfer.SinkOptions{OutPath: sinkOutPath, Stdout: out}
		if !toStdout {
			totalSize := pre.Size
			opts.Progress = func(received int64) { pr.Update(received, totalSize) }
			writtenAnnounce = sinkAnnounceName(pre, sinkOutPath)
		}
		sink, err := xfer.OpenSink(pre, opts)
		if err != nil {
			err = fmt.Errorf("opening sink: %w", err)
			done <- err
			return nil, err
		}
		return &finalizingSink{
			w: sink,
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
	fmt.Fprintf(stderr, "connection: %s\n", sess.Route())
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
}

// runRecvWatch dispatches to host or joiner mode based on whether a
// positional code was supplied:
//
//   - no code → host mode: reserve a persistent slot, print a stable
//     code, accept transfers from successive senders.
//   - one code → joiner mode: join an existing slot and stay paired,
//     accepting every transfer the peer pushes until they disconnect.
//
// Both modes write into outPath (defaulting to cwd), which must be a
// directory; per-transfer files land under their preamble-supplied
// filename, tar streams extract there, text payloads go to stdout.
func runRecvWatch(ctx context.Context, logger *slog.Logger, server string, policy client.RelayPolicy, outPath string, showQR bool, args []string, out, stderr io.Writer) error {
	dir, err := resolveWatchDir(outPath)
	if err != nil {
		return fmt.Errorf("running recv --watch: %w", err)
	}

	switch len(args) {
	case 0:
		return runRecvWatchHost(ctx, logger, server, policy, dir, showQR, out, stderr)
	case 1:
		return runRecvWatchJoiner(ctx, logger, server, policy, args[0], dir, out, stderr)
	default:
		return fmt.Errorf("usage: conduit recv --watch [<code>]")
	}
}

// resolveWatchDir validates outPath as the watch destination directory.
// Empty falls back to the working directory; a non-empty value must
// exist and be a directory (we drop multiple files into it under their
// preamble names — using a regular file would silently overwrite each
// previous transfer).
func resolveWatchDir(outPath string) (string, error) {
	if outPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		return cwd, nil
	}
	info, err := os.Stat(outPath)
	if err != nil {
		return "", fmt.Errorf("-o %q: %w", outPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("-o %q is not a directory", outPath)
	}
	return outPath, nil
}

// runRecvWatchJoiner joins the slot encoded by codeStr and stays paired
// across multiple transfers within that one pairing — the within-pairing
// reuse that web↔web has always had, just exposed through `recv` as
// well. The loop ends when the peer's session pump exits (clean EOF on
// disconnect) or ctx is cancelled.
//
// Unlike host mode this requires no server-side persistence: the
// existing rtc.Session multiplexes transfers on a single data channel,
// so as long as the peer keeps the WebSocket alive the receiver can
// keep consuming preambles.
func runRecvWatchJoiner(ctx context.Context, logger *slog.Logger, server string, policy client.RelayPolicy, codeStr, dir string, out, stderr io.Writer) error {
	code, err := wire.ParseCode(codeStr)
	if err != nil {
		return fmt.Errorf("running recv --watch: %w", err)
	}
	if len(code.Words) == 0 {
		return fmt.Errorf("running recv --watch: code %q is missing the word portion", codeStr)
	}
	sess, err := client.OpenReceiver(ctx, logger, server, code, policy, makeWatchSinkOpener(dir, out, stderr))
	if err != nil {
		return fmt.Errorf("running recv --watch: %w", err)
	}
	defer sess.Close(ctx)
	fmt.Fprintf(stderr, "connection: %s\n", sess.Route())
	fmt.Fprintln(out, "watch mode: receiving until peer disconnects (ctrl-c to stop)")

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- sess.PumpErr() }()
	select {
	case pumpErr := <-pumpDone:
		if pumpErr != nil && !errors.Is(pumpErr, io.EOF) && !errors.Is(pumpErr, context.Canceled) {
			return fmt.Errorf("running recv --watch: %w", pumpErr)
		}
		return nil
	case <-ctx.Done():
		return nil
	}
}

// runRecvWatchHost reserves a persistent slot once, prints a stable
// code, and accepts transfers from successive senders until ctx is
// cancelled. Within each pairing the peer may push multiple transfers
// (e.g. the browser sender dropping several files); the loop advances
// to the next sender when the peer disconnects.
func runRecvWatchHost(ctx context.Context, logger *slog.Logger, server string, policy client.RelayPolicy, dir string, showQR bool, out, stderr io.Writer) error {
	var codeOnce sync.Once
	onCode := func(code string) {
		codeOnce.Do(func() {
			fmt.Fprintf(out, "code: %s\n", code)
			page, pageErr := receivePageURL(server, code)
			if pageErr == nil {
				fmt.Fprintf(out, "send from the browser: %s\n", page)
			}
			if showQR && pageErr == nil {
				if err := renderQR(out, page); err != nil {
					fmt.Fprintf(stderr, "qr render failed: %v\n", err)
				}
			}
			fmt.Fprintln(out, "watch mode: code stays valid for repeated receives until ctrl-c")
		})
	}
	host, err := client.OpenHost(ctx, logger, server, policy, onCode)
	if err != nil {
		return fmt.Errorf("running recv --watch: %w", err)
	}
	defer host.Close()

	for ctx.Err() == nil {
		fmt.Fprintln(out, "waiting for sender...")
		sess, err := host.Accept(ctx, makeWatchSinkOpener(dir, out, stderr))
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("running recv --watch: %w", err)
		}
		fmt.Fprintf(stderr, "connection: %s\n", sess.Route())
		// Wait for the peer to disconnect (pump exits on EOF) or for
		// the user to ctrl-c. PumpErr blocks on the same goroutine
		// that drives inbound transfer dispatch, so by the time it
		// returns every transfer the peer pushed has run to its sink's
		// Close.
		pumpDone := make(chan error, 1)
		go func() { pumpDone <- sess.PumpErr() }()
		select {
		case pumpErr := <-pumpDone:
			sess.Close(ctx)
			if pumpErr != nil && !errors.Is(pumpErr, io.EOF) && !errors.Is(pumpErr, context.Canceled) {
				return fmt.Errorf("running recv --watch: %w", pumpErr)
			}
		case <-ctx.Done():
			sess.Close(ctx)
			return nil
		}
	}
	return nil
}

// makeWatchSinkOpener builds a SinkOpener that drops each inbound transfer
// into dir, prints a per-transfer progress line, and announces "wrote
// <name>" on completion. Tar streams extract under dir; text payloads go
// to stdout (matching the one-shot recv default). Multiple invocations
// per session are expected — one per transfer the peer pushes.
func makeWatchSinkOpener(dir string, out, stderr io.Writer) client.SinkOpener {
	return func(pre wire.Preamble) (io.WriteCloser, error) {
		sinkOutPath := watchSinkOutPath(pre, dir)
		toStdout := sinkWritesToStdout(pre, sinkOutPath)
		pr := newProgressLine(stderr, "↓")
		var announce string
		opts := xfer.SinkOptions{OutPath: sinkOutPath, Stdout: out}
		if !toStdout {
			totalSize := pre.Size
			opts.Progress = func(received int64) { pr.Update(received, totalSize) }
			announce = sinkAnnounceName(pre, sinkOutPath)
		}
		sink, err := xfer.OpenSink(pre, opts)
		if err != nil {
			return nil, fmt.Errorf("opening sink: %w", err)
		}
		return &watchSink{
			w:        sink,
			pr:       pr,
			toStdout: toStdout,
			announce: announce,
			out:      out,
		}, nil
	}
}

// watchSinkOutPath maps a preamble to the OpenSink OutPath argument when
// the watch loop's user-facing root is a directory: regular files become
// "<dir>/<basename>", tar streams use the dir directly (OpenSink extracts
// into it), and text uses "" so OpenSink falls through to stdout.
func watchSinkOutPath(pre wire.Preamble, dir string) string {
	switch pre.Kind {
	case wire.PreambleKindFile:
		name := filepath.Base(pre.Name)
		if name == "" || name == "." || name == "/" {
			return ""
		}
		return filepath.Join(dir, name)
	case wire.PreambleKindTar:
		return dir
	default:
		return ""
	}
}

// watchSink wraps the per-transfer xfer.OpenSink output with a final
// "wrote <name>" announcement. Used by recv --watch where each transfer
// within the session should land independently. Progress is driven via
// xfer.SinkOptions.Progress on the underlying sink, not here.
type watchSink struct {
	w        io.WriteCloser
	pr       *progressLine
	toStdout bool
	announce string
	out      io.Writer
}

func (w *watchSink) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	if err != nil {
		return n, fmt.Errorf("writing watch sink: %w", err)
	}
	return n, nil
}

func (w *watchSink) Close() error {
	err := w.w.Close()
	if w.toStdout {
		w.pr.Done()
	} else {
		w.pr.Finish(" ✓")
		if w.announce != "" {
			fmt.Fprintf(w.out, "wrote %s\n", w.announce)
		}
	}
	return err
}

// openSource resolves the payload Source from --text, positional paths, or "-"
// stdin. Exactly one route must apply; mixing --text with paths is an error.
func openSource(text string, args []string, stdin io.Reader, git bool, opts xfer.SourceOptions) (*xfer.Source, error) {
	if text != "" && len(args) > 0 {
		return nil, fmt.Errorf("resolving send source: pass either --text or a path, not both")
	}
	if text != "" {
		src, err := xfer.OpenText(text, opts)
		if err != nil {
			return nil, fmt.Errorf("resolving send source: %w", err)
		}
		return src, nil
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("resolving send source: pass --text <message>, one or more paths, or '-' for stdin")
	}
	src, err := xfer.OpenPaths(args, stdin, git, opts)
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
	start    time.Time
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
	if p.start.IsZero() {
		p.start = now
	}
	// Always emit when the transfer reaches its known total so the user sees a
	// final 100% line; the throttle would otherwise suppress an ack that lands
	// in the same window as the prior update.
	final := total > 0 && done >= total
	if !final && p.any && now.Sub(p.last) < progressMinInterval {
		return
	}
	p.last = now
	p.any = true
	p.write(formatProgress(p.prefix, done, total, now.Sub(p.start)))
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

// formatProgress renders the progress line as
// "↑ 5.0 MB / 50.0 MB (10%) · 2.1 MB/s · ETA 21s". Rate and ETA are appended
// only once enough has elapsed to compute them meaningfully — the rate term
// is suppressed for the first ~250ms so the initial ack doesn't render an
// inflated number, and ETA is dropped when the total is unknown.
func formatProgress(prefix string, done, total int64, elapsed time.Duration) string {
	var s string
	if total > 0 {
		pct := float64(done) / float64(total) * 100
		s = fmt.Sprintf("%s %s / %s (%.0f%%)", prefix, humanBytes(done), humanBytes(total), pct)
	} else {
		s = fmt.Sprintf("%s %s", prefix, humanBytes(done))
	}
	const minElapsedForRate = 250 * time.Millisecond
	if done > 0 && elapsed >= minElapsedForRate {
		rate := float64(done) / elapsed.Seconds()
		if rate >= 1 {
			s += fmt.Sprintf(" · %s/s", humanBytes(int64(rate)))
			if total > done {
				etaSec := float64(total-done) / rate
				s += " · ETA " + humanDuration(time.Duration(etaSec * float64(time.Second)))
			}
		}
	}
	return s
}

// humanDuration formats d as a compact, monotonically-shrinking ETA string:
// sub-second collapses to "<1s", under a minute is "Ns", under an hour is
// "MmSSs" (or "Mm" if seconds are zero), and longer is "HhMMm".
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d - time.Duration(m)*time.Minute) / time.Second)
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d - time.Duration(h)*time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%02dm", h, m)
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

// finalizingSink wraps a writable sink with an onClose hook fired after
// the sink's own Close. The receive command uses this to learn when the
// first transfer's payload has been fully flushed to disk so it can return
// out of its Wait-for-first-transfer loop. Progress is driven via
// xfer.SinkOptions.Progress on the underlying sink, not here.
type finalizingSink struct {
	w       io.WriteCloser
	onClose func() error
}

func (p *finalizingSink) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if err != nil {
		return n, fmt.Errorf("writing finalizing sink: %w", err)
	}
	return n, nil
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
