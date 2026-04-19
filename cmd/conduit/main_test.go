package main

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/signaling"
)

func TestSendRecvRoundTrip(t *testing.T) {
	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(5*time.Second),
		signaling.WithHelloTimeout(5*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	sendOut := &syncBuffer{}
	recvOut := &syncBuffer{}

	type result struct {
		out string
		err error
	}
	sendCtx, cancelSend := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancelSend()
	sendDone := make(chan result, 1)
	wg.Go(func() {
		err := mainE(sendCtx, logger, sendOut, []string{"send", "--server", ts.URL, "--text", "hello conduit"})
		sendDone <- result{out: sendOut.String(), err: err}
	})

	// Poll sender output for the reserved code line.
	var code string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, ok := extractCode(sendOut.String())
		if ok {
			code = line
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code == "" {
		t.Fatalf("did not observe code in sender output; got=%q", sendOut.String())
	}

	if err := mainE(ctx, logger, recvOut, []string{"recv", "--server", ts.URL, code}); err != nil {
		t.Fatalf("recv: %v (sender out so far: %q)", err, sendOut.String())
	}
	if got := strings.TrimSpace(recvOut.String()); got != "hello conduit" {
		t.Errorf("recv output = %q, want %q", got, "hello conduit")
	}

	select {
	case r := <-sendDone:
		if r.err != nil {
			t.Errorf("send err = %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("send did not complete")
	}
}

// extractCode pulls the numeric slot out of the "code: N" line.
func extractCode(out string) (string, bool) {
	const prefix = "code: "
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix)), true
		}
	}
	return "", false
}

// syncBuffer is a concurrency-safe bytes.Buffer; the sender goroutine writes
// while the test's main goroutine reads.
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

func TestParseCode(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		err  bool
	}{
		{"42", 42, false},
		{"42-ice-cream-monkey", 42, false},
		{"1", 1, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
	}
	for _, c := range cases {
		got, err := parseCode(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseCode(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCode(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseCode(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestWsURLFor(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"http://localhost:8080", "ws://localhost:8080/ws", false},
		{"https://example.com", "wss://example.com/ws", false},
		{"ws://example.com/", "ws://example.com/ws", false},
		{"wss://example.com/base", "wss://example.com/base/ws", false},
		{"example.com", "", true},
		{"ftp://example.com", "", true},
	}
	for _, c := range cases {
		got, err := wsURLFor(c.in)
		if c.err {
			if err == nil {
				t.Errorf("wsURLFor(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("wsURLFor(%q) err = %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("wsURLFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
