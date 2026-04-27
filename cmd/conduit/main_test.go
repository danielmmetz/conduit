package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/danielmmetz/conduit/internal/wire"
	"golang.org/x/sync/errgroup"
)

// Full-stack send/recv tests below spin up real pion PeerConnections (UDP/ICE
// on loopback). Running them with t.Parallel() against each other routinely
// caused flakes before ICE was tuned for local candidates; the rest of this
// package stays parallel. internal/rtc tests use an in-memory MsgConn only and
// do not need this serialization.
func TestSendRecvRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var sendBuf, recvBuf syncBuffer

	codeCh := make(chan string, 1)
	sendOut := &codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, nil, sendOut, io.Discard, []string{"send", "--server", ts.URL, "--text", "hello conduit"}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var code string
		select {
		case c, ok := <-codeCh:
			if !ok {
				return fmt.Errorf("recv: sender exited before code was available")
			}
			code = c
		case <-gctx.Done():
			return fmt.Errorf("recv: %w", gctx.Err())
		}
		if err := mainE(gctx, logger, nil, &recvBuf, io.Discard, []string{"recv", "--server", ts.URL, code}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("send/recv: %v (sender out=%q)", err, sendBuf.String())
	}
	if got := strings.TrimSpace(recvBuf.String()); got != "hello conduit" {
		t.Errorf("recv output = %q, want %q", got, "hello conduit")
	}
}

func TestSendRecvFileRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	tmp := t.TempDir()
	sendPath := filepath.Join(tmp, "payload.bin")
	payload := bytes.Repeat([]byte("conduit phase 4 file payload\n"), 512)
	if err := os.WriteFile(sendPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	recvPath := filepath.Join(tmp, "received.bin")

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var sendBuf syncBuffer

	codeCh := make(chan string, 1)
	sendOut := &codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, nil, sendOut, io.Discard, []string{"send", "--server", ts.URL, sendPath}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var code string
		select {
		case c, ok := <-codeCh:
			if !ok {
				return fmt.Errorf("recv: sender exited before code was available")
			}
			code = c
		case <-gctx.Done():
			return fmt.Errorf("recv: %w", gctx.Err())
		}
		if err := mainE(gctx, logger, nil, io.Discard, io.Discard, []string{"recv", "--server", ts.URL, "-o", recvPath, code}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("send/recv: %v (sender out=%q)", err, sendBuf.String())
	}
	got, err := os.ReadFile(recvPath)
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("received %d bytes, want %d (prefix recv %q want %q)",
			len(got), len(payload), truncateForLog(got, 64), truncateForLog(payload, 64))
	}
}

// TestRecvWatchMultipleSenders exercises the --watch host loop: one
// receiver reserves a persistent slot, prints one stable code, and lands
// payloads from two sequential senders in the watch directory. The
// senders drive the joiner side via client.OpenReceiver — that's what
// the browser sender does in the motivating workflow (WASM's
// session-mode openReceiver joins a slot and pushes via the resulting
// Session).
func TestRecvWatchMultipleSenders(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	dir := t.TempDir()
	codeCh := make(chan string, 4)
	var recvBuf syncBuffer
	recvOut := &codeWatchBuffer{buf: &recvBuf, ch: codeCh}

	recvDone := make(chan error, 1)
	go func() {
		recvDone <- mainE(ctx, logger, nil, recvOut, io.Discard,
			[]string{"recv", "--server", ts.URL, "--watch", "-o", dir})
		close(codeCh)
	}()

	var hostCode string
	select {
	case code, ok := <-codeCh:
		if !ok {
			t.Fatalf("recv --watch exited before code was available (out=%q)", recvBuf.String())
		}
		hostCode = code
	case <-ctx.Done():
		t.Fatal("timed out waiting for code")
	}
	parsed, err := wire.ParseCode(hostCode)
	if err != nil {
		t.Fatalf("parse code %q: %v", hostCode, err)
	}

	for i := range 2 {
		payload := fmt.Appendf(nil, "payload from sender %d\n", i)
		name := fmt.Sprintf("sender-%d.bin", i)
		sess, err := client.OpenReceiver(ctx, logger, ts.URL, parsed, client.RelayAuto, nil)
		if err != nil {
			t.Fatalf("sender %d open: %v", i, err)
		}
		pre := wire.Preamble{
			Kind: wire.PreambleKindFile,
			Name: name,
			Size: int64(len(payload)),
			MIME: "application/octet-stream",
		}
		if err := sess.Push(ctx, pre, bytes.NewReader(payload)); err != nil {
			sess.Close(ctx)
			t.Fatalf("sender %d push: %v", i, err)
		}
		if err := sess.Close(ctx); err != nil {
			t.Fatalf("sender %d close: %v", i, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("sender %d readfile: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("sender %d roundtrip: got %q, want %q", i, got, payload)
		}
	}

	// No further host codes should have been emitted; the watch loop
	// reuses one stable code across senders.
	select {
	case extra, ok := <-codeCh:
		if ok {
			t.Errorf("extra code emitted: %q (codes should be stable)", extra)
		}
	default:
	}

	cancel()
	select {
	case err := <-recvDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("recv --watch returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("recv --watch did not exit after cancel (out=%q)", recvBuf.String())
	}
}

// TestRecvWatchJoinerMultiplePushes covers the joiner-mode of
// `recv --watch <code>`: the receiver joins an existing slot and stays
// paired across multiple transfers, exiting only when the peer
// disconnects. Mirrors what the browser sender does — open a session,
// push a few files, close the tab — but using the Go client to drive
// the sender side directly.
func TestRecvWatchJoinerMultiplePushes(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	dir := t.TempDir()
	codeCh := make(chan string, 1)
	senderDone := make(chan error, 1)

	// Sender side: reserve a non-persistent slot (the way the browser
	// does today), block in OpenSender until the CLI joins, then push
	// two transfers in the same pairing and close.
	go func() {
		sess, err := client.OpenSender(ctx, logger, ts.URL, client.RelayAuto, func(code string) {
			codeCh <- code
		}, nil)
		if err != nil {
			senderDone <- err
			return
		}
		defer sess.Close(ctx)
		for i := range 2 {
			payload := fmt.Appendf(nil, "transfer %d\n", i)
			pre := wire.Preamble{
				Kind: wire.PreambleKindFile,
				Name: fmt.Sprintf("transfer-%d.bin", i),
				Size: int64(len(payload)),
				MIME: "application/octet-stream",
			}
			if err := sess.Push(ctx, pre, bytes.NewReader(payload)); err != nil {
				senderDone <- err
				return
			}
		}
		senderDone <- nil
	}()

	var code string
	select {
	case c := <-codeCh:
		code = c
	case <-ctx.Done():
		t.Fatal("timed out waiting for code")
	}

	// CLI: join via --watch and stay paired until the sender closes.
	recvDone := make(chan error, 1)
	go func() {
		recvDone <- mainE(ctx, logger, nil, io.Discard, io.Discard,
			[]string{"recv", "--server", ts.URL, "--watch", "-o", dir, code})
	}()

	if err := <-senderDone; err != nil {
		t.Fatalf("sender: %v", err)
	}
	// Sender's defer sess.Close runs as the goroutine exits; the
	// receiver's pump should see EOF and the watch loop returns.
	select {
	case err := <-recvDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("recv --watch returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recv --watch did not exit after sender close")
	}

	for i := range 2 {
		name := fmt.Sprintf("transfer-%d.bin", i)
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("transfer %d readfile: %v", i, err)
			continue
		}
		want := fmt.Appendf(nil, "transfer %d\n", i)
		if !bytes.Equal(got, want) {
			t.Errorf("transfer %d: got %q, want %q", i, got, want)
		}
	}
}

// TestRecvWatchRejectsTooManyArgs confirms --watch refuses extra
// positional arguments past the optional code.
func TestRecvWatchRejectsTooManyArgs(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	err := mainE(t.Context(), logger, nil, io.Discard, io.Discard,
		[]string{"recv", "--watch", "42-foo-bar-baz", "extra"})
	if err == nil {
		t.Fatal("recv --watch <code> extra returned nil, want error")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("error %q does not mention usage", err)
	}
}

func TestRelayPolicySet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want client.RelayPolicy
	}{
		{"auto", client.RelayAuto},
		{"never", client.RelayNone},
		{"always", client.RelayOnly},
	}
	for _, c := range cases {
		var got client.RelayPolicy
		if err := got.Set(c.in); err != nil {
			t.Errorf("RelayPolicy.Set(%q) err = %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("RelayPolicy.Set(%q) = %v, want %v", c.in, got, c.want)
		}
		if rt := got.String(); rt != c.in {
			t.Errorf("RelayPolicy(%v).String() = %q, want %q", got, rt, c.in)
		}
	}
}

func TestRelayPolicySetInvalid(t *testing.T) {
	t.Parallel()
	var p client.RelayPolicy
	if err := p.Set("bogus"); err == nil {
		t.Fatal("RelayPolicy.Set(\"bogus\") err = nil, want error")
	}
}

// TestSendRecvDirectoryRoundTrip covers the directory-tar shape: the sender
// streams a PAX tar of a small tree and the receiver extracts it into a
// separate output directory, verifying both files and their contents
// round-trip. This exercises the tar producer goroutine, the preamble
// (kind="tar"), and the traversal-safe extractor together.
func TestSendRecvDirectoryRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	srcRoot := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(srcRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "top.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "sub", "nested.txt"), []byte("bravo"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dstRoot := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var sendBuf syncBuffer
	codeCh := make(chan string, 1)
	sendOut := &codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, nil, sendOut, io.Discard, []string{"send", "--server", ts.URL, srcRoot}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var code string
		select {
		case c, ok := <-codeCh:
			if !ok {
				return fmt.Errorf("recv: sender exited before code was available")
			}
			code = c
		case <-gctx.Done():
			return fmt.Errorf("recv: %w", gctx.Err())
		}
		if err := mainE(gctx, logger, nil, io.Discard, io.Discard, []string{"recv", "--server", ts.URL, "-o", dstRoot, code}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("send/recv dir: %v (sender out=%q)", err, sendBuf.String())
	}

	base := filepath.Base(srcRoot)
	top, err := os.ReadFile(filepath.Join(dstRoot, base, "top.txt"))
	if err != nil {
		t.Fatalf("read top.txt: %v", err)
	}
	if string(top) != "alpha" {
		t.Errorf("top.txt = %q, want %q", top, "alpha")
	}
	nested, err := os.ReadFile(filepath.Join(dstRoot, base, "sub", "nested.txt"))
	if err != nil {
		t.Fatalf("read nested.txt: %v", err)
	}
	if string(nested) != "bravo" {
		t.Errorf("nested.txt = %q, want %q", nested, "bravo")
	}
}

// TestSendRecvStdinStdoutRoundTrip exercises the streaming-stdin send source
// (Size=-1) and the positional "-" stdout marker on the receiver. Because
// stdin can't be seeked or pre-sized, the preamble should record size=-1 and
// the payload should still round-trip intact end-to-end.
func TestSendRecvStdinStdoutRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Longer budget than the other full-stack tests: stdin has been the first
	// to tip over when ICE convergence slows down alongside concurrent rtc
	// test packages in `go test ./...`.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	payload := bytes.Repeat([]byte("stdin payload chunk\n"), 64)
	stdin := bytes.NewReader(payload)

	var sendBuf, recvBuf syncBuffer
	codeCh := make(chan string, 1)
	sendOut := &codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, stdin, sendOut, io.Discard, []string{"send", "--server", ts.URL, "-"}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var code string
		select {
		case c, ok := <-codeCh:
			if !ok {
				return fmt.Errorf("recv: sender exited before code was available")
			}
			code = c
		case <-gctx.Done():
			return fmt.Errorf("recv: %w", gctx.Err())
		}
		// "-" positional means write payload to the out stream (stdout here).
		if err := mainE(gctx, logger, nil, &recvBuf, io.Discard, []string{"recv", "--server", ts.URL, code, "-"}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("send/recv stdin: %v (sender out=%q)", err, sendBuf.String())
	}
	if !bytes.Equal(recvBuf.buf.Bytes(), payload) {
		t.Fatalf("stdin round trip mismatch: got %d bytes, want %d", recvBuf.buf.Len(), len(payload))
	}
}

// TestSendRecvMultiFileRoundTrip verifies multi-path send (→ tar stream)
// extracts both files on the receive side under the caller-provided root.
func TestSendRecvMultiFileRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(2*time.Second),
		signaling.WithHelloTimeout(2*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	src := t.TempDir()
	p1 := filepath.Join(src, "one.txt")
	p2 := filepath.Join(src, "two.txt")
	if err := os.WriteFile(p1, []byte("first"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(p2, []byte("second payload"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := t.TempDir()

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var sendBuf syncBuffer
	codeCh := make(chan string, 1)
	sendOut := &codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, nil, sendOut, io.Discard, []string{"send", "--server", ts.URL, p1, p2}); err != nil {
			return fmt.Errorf("send: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		var code string
		select {
		case c, ok := <-codeCh:
			if !ok {
				return fmt.Errorf("recv: sender exited before code was available")
			}
			code = c
		case <-gctx.Done():
			return fmt.Errorf("recv: %w", gctx.Err())
		}
		if err := mainE(gctx, logger, nil, io.Discard, io.Discard, []string{"recv", "--server", ts.URL, "-o", dst, code}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("send/recv multi-file: %v (sender out=%q)", err, sendBuf.String())
	}

	one, err := os.ReadFile(filepath.Join(dst, "one.txt"))
	if err != nil || string(one) != "first" {
		t.Errorf("one.txt = %q err=%v, want %q", one, err, "first")
	}
	two, err := os.ReadFile(filepath.Join(dst, "two.txt"))
	if err != nil || string(two) != "second payload" {
		t.Errorf("two.txt = %q err=%v, want %q", two, err, "second payload")
	}
}

func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

func TestReceivePageURL(t *testing.T) {
	t.Parallel()
	u, err := receivePageURL("http://localhost:8080", "7-foo-bar-baz")
	if err != nil {
		t.Fatal(err)
	}
	const want = "http://localhost:8080#7-foo-bar-baz"
	if u != want {
		t.Fatalf("receivePageURL: got %q, want %q", u, want)
	}
}

func TestFormatProgressKnownTotal(t *testing.T) {
	t.Parallel()
	// 5 MiB done out of 50 MiB after 1 second → 5 MB/s, 9s ETA.
	got := formatProgress("↑", 5*1024*1024, 50*1024*1024, time.Second)
	const want = "↑ 5.0 MB / 50.0 MB (10%) · 5.0 MB/s · ETA 9s"
	if got != want {
		t.Errorf("formatProgress: got %q, want %q", got, want)
	}
}

func TestFormatProgressUnknownTotal(t *testing.T) {
	t.Parallel()
	// total<0 means streaming source (stdin/tar): rate shows, ETA omitted.
	got := formatProgress("↓", 2*1024*1024, -1, 2*time.Second)
	const want = "↓ 2.0 MB · 1.0 MB/s"
	if got != want {
		t.Errorf("formatProgress: got %q, want %q", got, want)
	}
}

func TestFormatProgressEarlySuppressesRate(t *testing.T) {
	t.Parallel()
	// First ~250ms: rate would be inflated by setup overhead. Suppress so the
	// user doesn't see "↑ 1.0 MB / 50.0 MB (2%) · 200 MB/s · ETA <1s" then
	// settle to the real number two updates later.
	got := formatProgress("↑", 1024*1024, 50*1024*1024, 100*time.Millisecond)
	const want = "↑ 1.0 MB / 50.0 MB (2%)"
	if got != want {
		t.Errorf("formatProgress: got %q, want %q", got, want)
	}
}

func TestFormatProgressFinalLine(t *testing.T) {
	t.Parallel()
	// At completion, total-done is 0 → ETA term is omitted but rate stays.
	got := formatProgress("↑", 50*1024*1024, 50*1024*1024, 10*time.Second)
	const want = "↑ 50.0 MB / 50.0 MB (100%) · 5.0 MB/s"
	if got != want {
		t.Errorf("formatProgress: got %q, want %q", got, want)
	}
}

func TestHumanDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "<1s"},
		{time.Second, "1s"},
		{45 * time.Second, "45s"},
		{time.Minute, "1m"},
		{90 * time.Second, "1m30s"},
		{59*time.Minute + 30*time.Second, "59m30s"},
		{time.Hour, "1h"},
		{2*time.Hour + 5*time.Minute, "2h05m"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderQR(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := renderQR(&buf, "https://conduit.danielmmetz.com#7-foo-bar-baz"); err != nil {
		t.Fatalf("renderQR: %v", err)
	}
	out := buf.String()
	// Expect at least one full block and the trailing newlines from a
	// rectangular grid; if either is missing the renderer is broken.
	if !strings.ContainsRune(out, '█') {
		t.Errorf("output is missing dark modules; got %q", truncateForLog([]byte(out), 80))
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 8 {
		t.Errorf("got %d lines, want at least 8", len(lines))
	}
	width := len([]rune(lines[0]))
	for i, ln := range lines {
		if got := len([]rune(ln)); got != width {
			t.Errorf("line %d width %d, want %d (output not rectangular)", i, got, width)
			break
		}
	}
}

// extractCode pulls the numeric slot out of the "code: N" line.
func extractCode(out string) (string, bool) {
	const prefix = "code: "
	for line := range strings.SplitSeq(out, "\n") {
		if after, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(after), true
		}
	}
	return "", false
}

// codeNotifyBuffer forwards writes to buf and delivers the first full "code:"
// line on ch (used to start recv without polling send output).
type codeNotifyBuffer struct {
	buf  *syncBuffer
	ch   chan<- string
	sent atomic.Bool
}

func (w *codeNotifyBuffer) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	if w.sent.Load() {
		return n, nil
	}
	if code, ok := extractCode(w.buf.String()); ok {
		if w.sent.CompareAndSwap(false, true) {
			w.ch <- code
		}
	}
	return n, nil
}

// codeWatchBuffer forwards writes to buf and delivers every "code:" line on
// ch as the watch loop emits a fresh slot per iteration. Each line appears
// at most once (deduped against the cumulative buffer prefix that was
// already scanned).
type codeWatchBuffer struct {
	buf       *syncBuffer
	ch        chan<- string
	mu        sync.Mutex
	scanUntil int
}

func (w *codeWatchBuffer) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if err != nil {
		return n, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	full := w.buf.String()
	const prefix = "code: "
	for {
		idx := strings.Index(full[w.scanUntil:], prefix)
		if idx < 0 {
			break
		}
		start := w.scanUntil + idx
		nl := strings.IndexByte(full[start:], '\n')
		if nl < 0 {
			break
		}
		line := full[start : start+nl]
		w.scanUntil = start + nl + 1
		w.ch <- strings.TrimSpace(strings.TrimPrefix(line, prefix))
	}
	return n, nil
}

// syncBuffer is a concurrency-safe bytes.Buffer shared by the codeNotifyBuffer
// and the test after wg.Wait.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

