package main

import (
	"bytes"
	"context"
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

	"github.com/danielmmetz/conduit/internal/signaling"
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
		if err := mainE(gctx, logger, sendOut, []string{"send", "--server", ts.URL, "--text", "hello conduit"}); err != nil {
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
		if err := mainE(gctx, logger, &recvBuf, []string{"recv", "--server", ts.URL, code}); err != nil {
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
		if err := mainE(gctx, logger, sendOut, []string{"send", "--server", ts.URL, sendPath}); err != nil {
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
		if err := mainE(gctx, logger, io.Discard, []string{"recv", "--server", ts.URL, "-o", recvPath, code}); err != nil {
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

func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
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

func TestWsURLForValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"http://localhost:8080", "ws://localhost:8080/ws"},
		{"https://example.com", "wss://example.com/ws"},
		{"ws://example.com/", "ws://example.com/ws"},
		{"wss://example.com/base", "wss://example.com/base/ws"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := wsURLFor(c.in)
			if err != nil {
				t.Fatalf("wsURLFor(%q) err = %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("wsURLFor(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestWsURLForInvalid(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"example.com", "ftp://example.com"} {
		in := in
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := wsURLFor(in)
			if err == nil {
				t.Fatalf("wsURLFor(%q) = %q, want error", in, got)
			}
		})
	}
}
