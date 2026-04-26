package signaling_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/ratelimit"
	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/danielmmetz/conduit/internal/turnauth"
	"github.com/danielmmetz/conduit/internal/wire"
)

type testHarness struct {
	ts     *httptest.Server
	server *signaling.Server
}

func newTestHarness(t *testing.T, opts ...signaling.Option) *testHarness {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	defaults := []signaling.Option{
		signaling.WithSlotTTL(2 * time.Second),
		signaling.WithHelloTimeout(2 * time.Second),
	}
	srv := signaling.NewServer(logger, append(defaults, opts...)...)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	mux.HandleFunc("GET /healthz", srv.HandleHealthz)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &testHarness{ts: ts, server: srv}
}

func (h *testHarness) dial(ctx context.Context, t *testing.T) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func writeJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readEnvelope(t *testing.T, ctx context.Context, conn *websocket.Conn) wire.Envelope {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env wire.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return env
}

func TestHealthz(t *testing.T) {
	h := newTestHarness(t)
	resp, err := h.ts.Client().Get(h.ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Errorf("body = %q, want contains %q", body, "ok")
	}
}

// countingIssuer mints a distinct credential on every Issue call. The
// per-call distinction lets tests verify that each peer in a pairing gets
// its own credential — sharing one credential between peers silently breaks
// TURN allocation for the second peer (Cloudflare and other providers bind
// a credential to a single Allocate).
type countingIssuer struct {
	mu  sync.Mutex
	n   int
	uri string
}

func (c *countingIssuer) Issue(context.Context) (turnauth.Creds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return turnauth.Creds{
		URIs:       []string{c.uri},
		Username:   fmt.Sprintf("user-%d", c.n),
		Credential: fmt.Sprintf("cred-%d", c.n),
		TTL:        60,
	}, nil
}

func TestPairedIncludesPerPeerTurn(t *testing.T) {
	iss := &countingIssuer{uri: "turn:example.com:3478"}
	h := newTestHarness(t, signaling.WithTurnIssuer(iss))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sender := h.dial(ctx, t)
	writeJSON(t, ctx, sender, wire.ClientHello{Op: wire.OpReserve})

	reserved := readEnvelope(t, ctx, sender)
	if reserved.Op != wire.OpReserved {
		t.Fatalf("op = %q, want %q", reserved.Op, wire.OpReserved)
	}

	receiver := h.dial(ctx, t)
	writeJSON(t, ctx, receiver, wire.ClientHello{Op: wire.OpJoin, Slot: reserved.Slot})

	sPaired := readEnvelope(t, ctx, sender)
	rPaired := readEnvelope(t, ctx, receiver)
	for _, label := range []struct {
		name string
		env  wire.Envelope
	}{
		{"sender", sPaired},
		{"receiver", rPaired},
	} {
		if label.env.Op != wire.OpPaired {
			t.Fatalf("%s paired op = %q, want %q", label.name, label.env.Op, wire.OpPaired)
		}
		if label.env.TURN == nil || len(label.env.TURN.URIs) != 1 {
			t.Fatalf("%s paired TURN = %+v, want non-empty URIs", label.name, label.env.TURN)
		}
		if label.env.TURN.URIs[0] != "turn:example.com:3478" {
			t.Errorf("%s TURN uris[0] = %q, want turn:example.com:3478", label.name, label.env.TURN.URIs[0])
		}
		if label.env.TURN.Credential == "" || label.env.TURN.Username == "" {
			t.Fatalf("%s TURN missing username or credential", label.name)
		}
	}
	if sPaired.TURN.Credential == rPaired.TURN.Credential || sPaired.TURN.Username == rPaired.TURN.Username {
		t.Errorf("sender and receiver got the same credential: sender=%+v receiver=%+v", sPaired.TURN, rPaired.TURN)
	}
}

func TestReserveAndRelay(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sender := h.dial(ctx, t)
	writeJSON(t, ctx, sender, wire.ClientHello{Op: wire.OpReserve})

	reserved := readEnvelope(t, ctx, sender)
	if reserved.Op != wire.OpReserved {
		t.Fatalf("op = %q, want %q", reserved.Op, wire.OpReserved)
	}
	if reserved.Slot == 0 {
		t.Fatalf("slot = 0, want > 0")
	}

	receiver := h.dial(ctx, t)
	writeJSON(t, ctx, receiver, wire.ClientHello{Op: wire.OpJoin, Slot: reserved.Slot})

	// Both sides should see "paired".
	if got := readEnvelope(t, ctx, sender).Op; got != wire.OpPaired {
		t.Fatalf("sender paired op = %q, want %q", got, wire.OpPaired)
	}
	if got := readEnvelope(t, ctx, receiver).Op; got != wire.OpPaired {
		t.Fatalf("receiver paired op = %q, want %q", got, wire.OpPaired)
	}

	// Opaque bidirectional relay.
	if err := sender.Write(ctx, websocket.MessageText, []byte("hello from sender")); err != nil {
		t.Fatalf("sender write: %v", err)
	}
	_, data, err := receiver.Read(ctx)
	if err != nil {
		t.Fatalf("receiver read: %v", err)
	}
	if string(data) != "hello from sender" {
		t.Errorf("receiver got %q, want %q", data, "hello from sender")
	}

	if err := receiver.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02, 0x03}); err != nil {
		t.Fatalf("receiver write: %v", err)
	}
	typ, data, err := sender.Read(ctx)
	if err != nil {
		t.Fatalf("sender read: %v", err)
	}
	if typ != websocket.MessageBinary || string(data) != "\x01\x02\x03" {
		t.Errorf("sender got type=%v data=%x, want binary 010203", typ, data)
	}

	// Clean close from sender propagates to receiver.
	if err := sender.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("sender close: %v", err)
	}
	if _, _, err := receiver.Read(ctx); err == nil {
		t.Fatalf("receiver read after sender close returned nil error")
	} else if !isCleanClose(err) {
		t.Fatalf("receiver read error = %v, want clean close", err)
	}

	// Server-side slot cleanup is deferred after the handler returns; allow
	// a short window for ActiveSlots to drain.
	deadline := time.Now().Add(time.Second)
	for h.server.ActiveSlots() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.server.ActiveSlots(); got != 0 {
		t.Errorf("ActiveSlots = %d after close, want 0", got)
	}
}

func TestJoinUnknownSlot(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	receiver := h.dial(ctx, t)
	writeJSON(t, ctx, receiver, wire.ClientHello{Op: wire.OpJoin, Slot: 424242})
	env := readEnvelope(t, ctx, receiver)
	if env.Op != wire.OpError {
		t.Fatalf("op = %q, want %q", env.Op, wire.OpError)
	}
	if env.Code != wire.ErrSlotNotFound {
		t.Errorf("code = %q, want %q", env.Code, wire.ErrSlotNotFound)
	}
}

func TestSlotExpiry(t *testing.T) {
	h := newTestHarness(t, signaling.WithSlotTTL(200*time.Millisecond))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sender := h.dial(ctx, t)
	writeJSON(t, ctx, sender, wire.ClientHello{Op: wire.OpReserve})
	reserved := readEnvelope(t, ctx, sender)
	if reserved.Op != wire.OpReserved {
		t.Fatalf("op = %q, want %q", reserved.Op, wire.OpReserved)
	}

	// Without a receiver, the server should emit an error and close.
	env := readEnvelope(t, ctx, sender)
	if env.Op != wire.OpError || env.Code != wire.ErrExpired {
		t.Fatalf("env = %+v, want expired error", env)
	}
	if got := h.server.ActiveSlots(); got != 0 {
		t.Errorf("ActiveSlots = %d after expiry, want 0", got)
	}
}

func TestBadFirstFrame(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn := h.dial(ctx, t)
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	env := readEnvelope(t, ctx, conn)
	if env.Op != wire.OpError || env.Code != wire.ErrBadRequest {
		t.Fatalf("env = %+v, want bad_request error", env)
	}
}

func TestUnknownOp(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn := h.dial(ctx, t)
	writeJSON(t, ctx, conn, wire.ClientHello{Op: "relocate"})
	env := readEnvelope(t, ctx, conn)
	if env.Op != wire.OpError || env.Code != wire.ErrBadRequest {
		t.Fatalf("env = %+v, want bad_request error", env)
	}
}

func TestReserveRateLimited(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := ratelimit.KeyedLimiter{
		Rate:  0,
		Burst: 1,
		Now:   func() time.Time { return now },
	}
	h := newTestHarness(t, signaling.WithReserveLimiter(&limiter))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	sender := h.dial(ctx, t)
	writeJSON(t, ctx, sender, wire.ClientHello{Op: wire.OpReserve})
	if env := readEnvelope(t, ctx, sender); env.Op != wire.OpReserved {
		t.Fatalf("first reserve env = %+v, want reserved", env)
	}

	second := h.dial(ctx, t)
	writeJSON(t, ctx, second, wire.ClientHello{Op: wire.OpReserve})
	env := readEnvelope(t, ctx, second)
	if env.Op != wire.OpError || env.Code != wire.ErrRateLimited {
		t.Fatalf("second reserve env = %+v, want rate_limited error", env)
	}
}

func TestReserveCapacityReached(t *testing.T) {
	h := newTestHarness(t, signaling.WithMaxConcurrentSlots(1))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	first := h.dial(ctx, t)
	writeJSON(t, ctx, first, wire.ClientHello{Op: wire.OpReserve})
	if env := readEnvelope(t, ctx, first); env.Op != wire.OpReserved {
		t.Fatalf("first reserve env = %+v, want reserved", env)
	}

	second := h.dial(ctx, t)
	writeJSON(t, ctx, second, wire.ClientHello{Op: wire.OpReserve})
	env := readEnvelope(t, ctx, second)
	if env.Op != wire.OpError || env.Code != wire.ErrCapacity {
		t.Fatalf("second reserve env = %+v, want capacity error", env)
	}
}

// TestConcurrentReserves makes sure pairing is race-free under load.
func TestConcurrentReserves(t *testing.T) {
	h := newTestHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	const pairs = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for range pairs {
		wg.Go(func() {
			if err := runRoundTrip(ctx, h); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("round trip: %v", err)
	}
	// Server-side slot cleanup runs after the client close frame lands, so
	// allow a short window for ActiveSlots to drain.
	deadline := time.Now().Add(time.Second)
	for h.server.ActiveSlots() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.server.ActiveSlots(); got != 0 {
		t.Errorf("ActiveSlots = %d after all pairs, want 0", got)
	}
}

func runRoundTrip(ctx context.Context, h *testHarness) error {
	url := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/ws"
	sender, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dialing sender: %w", err)
	}
	defer sender.CloseNow()
	if err := writeCtl(ctx, sender, wire.ClientHello{Op: wire.OpReserve}); err != nil {
		return fmt.Errorf("sending reserve: %w", err)
	}
	reserved, err := readCtl(ctx, sender)
	if err != nil {
		return fmt.Errorf("reading reserved: %w", err)
	}
	if reserved.Op != wire.OpReserved {
		return fmt.Errorf("reserved op = %q", reserved.Op)
	}

	receiver, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dialing receiver: %w", err)
	}
	defer receiver.CloseNow()
	if err := writeCtl(ctx, receiver, wire.ClientHello{Op: wire.OpJoin, Slot: reserved.Slot}); err != nil {
		return fmt.Errorf("sending join: %w", err)
	}
	if p, err := readCtl(ctx, sender); err != nil {
		return fmt.Errorf("reading sender paired: %w", err)
	} else if p.Op != wire.OpPaired {
		return fmt.Errorf("sender paired op = %q", p.Op)
	}
	if p, err := readCtl(ctx, receiver); err != nil {
		return fmt.Errorf("reading receiver paired: %w", err)
	} else if p.Op != wire.OpPaired {
		return fmt.Errorf("receiver paired op = %q", p.Op)
	}

	payload := fmt.Sprintf("slot %d payload", reserved.Slot)
	if err := sender.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
		return fmt.Errorf("writing sender payload: %w", err)
	}
	_, got, err := receiver.Read(ctx)
	if err != nil {
		return fmt.Errorf("reading receiver payload: %w", err)
	}
	if string(got) != payload {
		return fmt.Errorf("got %q, want %q", got, payload)
	}
	_ = sender.Close(websocket.StatusNormalClosure, "")
	return nil
}

func writeCtl(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	return nil
}

func readCtl(ctx context.Context, conn *websocket.Conn) (wire.Envelope, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return wire.Envelope{}, fmt.Errorf("reading: %w", err)
	}
	var env wire.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return wire.Envelope{}, fmt.Errorf("unmarshaling: %w", err)
	}
	return env, nil
}

func isCleanClose(err error) bool {
	if err == nil {
		return true
	}
	status := websocket.CloseStatus(err)
	if status == websocket.StatusNormalClosure || status == websocket.StatusNoStatusRcvd || status == websocket.StatusGoingAway {
		return true
	}
	return errors.Is(err, io.EOF)
}
