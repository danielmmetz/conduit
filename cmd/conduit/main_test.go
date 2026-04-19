package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/danielmmetz/conduit/internal/turnauth"
	"github.com/danielmmetz/conduit/internal/turnserver"
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

// TestSendRecvForceRelayRoundTrip stands up a loopback TURN server alongside the
// signaling server and runs send/recv with --force-relay on both sides. This
// exercises the relay code path end-to-end: ICE gathers only relay candidates,
// so all SCTP traffic flows through pion-turn (not direct host pairs), and the
// credentials it authenticates with come from the same HMAC secret the
// signaling server uses in turnauth.Issue.
func TestSendRecvForceRelayRoundTrip(t *testing.T) {
	secret := "phase5-relay-secret"

	udpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	turnAddr := udpConn.LocalAddr().(*net.UDPAddr)

	turnSrv, err := turnserver.Start(turnserver.Config{
		Secret:      secret,
		RelayIP:     net.ParseIP("127.0.0.1"),
		BindAddress: "127.0.0.1",
		UDPListener: udpConn,
		LogWriter:   t.Output(),
	})
	if err != nil {
		t.Fatalf("turnserver start: %v", err)
	}
	t.Cleanup(func() {
		if err := turnSrv.Close(); err != nil {
			t.Errorf("turnserver close: %v", err)
		}
	})

	turnURI := fmt.Sprintf("turn:%s?transport=udp", turnAddr.String())
	iss, err := turnauth.NewIssuer([]byte(secret), []string{turnURI}, 5*time.Minute, "conduit", time.Now)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}

	srv := signaling.NewServer(
		slog.New(slog.NewTextHandler(t.Output(), nil)),
		signaling.WithSlotTTL(5*time.Second),
		signaling.WithHelloTimeout(5*time.Second),
		signaling.WithTurnIssuer(iss),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var sendBuf, recvBuf syncBuffer
	codeCh := make(chan string, 1)
	sendOut := codeNotifyBuffer{buf: &sendBuf, ch: codeCh}

	payload := "relayed via TURN on loopback"

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		defer close(codeCh)
		if err := mainE(gctx, logger, &sendOut, []string{"send", "--server", ts.URL, "--force-relay", "--text", payload}); err != nil {
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
		if err := mainE(gctx, logger, &recvBuf, []string{"recv", "--server", ts.URL, "--force-relay", code}); err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		t.Fatalf("force-relay send/recv: %v (sender out=%q)", err, sendBuf.String())
	}
	if got := strings.TrimSpace(recvBuf.String()); got != payload {
		t.Errorf("recv output = %q, want %q", got, payload)
	}
}

func TestRelayPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		noRelay, forceRelay bool
		want                client.RelayPolicy
	}{
		{false, false, client.RelayAuto},
		{true, false, client.RelayNone},
		{false, true, client.RelayOnly},
	}
	for _, c := range cases {
		got, err := client.RelayPolicyFromFlags(c.noRelay, c.forceRelay)
		if err != nil {
			t.Errorf("RelayPolicyFromFlags(%v, %v) err = %v", c.noRelay, c.forceRelay, err)
		}
		if got != c.want {
			t.Errorf("RelayPolicyFromFlags(%v, %v) = %v, want %v", c.noRelay, c.forceRelay, got, c.want)
		}
	}
}

func TestRelayPolicyConflict(t *testing.T) {
	t.Parallel()
	if _, err := client.RelayPolicyFromFlags(true, true); err == nil {
		t.Fatal("RelayPolicyFromFlags(true, true) err = nil, want error")
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

