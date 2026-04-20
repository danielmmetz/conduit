package rtc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/rtc"
	"github.com/danielmmetz/conduit/internal/wire"
)

func TestSendRecvRoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := make([]byte, wire.SessionKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}

	payload := bytes.Repeat([]byte("conduit phase 4 — p2p bytes\n"), 512)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	sendErr := make(chan error, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		sendErr <- rtc.Send(ctx, a, key, rtc.Config{Logger: logger.With("role", "send")}, bytes.NewReader(payload))
	})

	var got bytes.Buffer
	recvErr := rtc.Recv(ctx, b, key, rtc.Config{Logger: logger.With("role", "recv")}, &got)
	sErr := <-sendErr
	if recvErr != nil {
		t.Fatalf("recv: %v (send err=%v)", recvErr, sErr)
	}
	if sErr != nil {
		t.Fatalf("send: %v", sErr)
	}

	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d bytes", got.Len(), len(payload))
	}
}

// TestSendReceivesAcks confirms the sender's OnRemoteProgress callback is
// fired with a non-zero cumulative byte count once the receiver has fully
// drained the payload. We can't assert a specific number of acks because
// threshold-based emission + the receiver's final Flush together mean one
// call for a small payload, multiple for a big one — but the final total
// must match the payload size for correctness.
func TestSendReceivesAcks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := make([]byte, wire.SessionKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// Exceed the ack threshold (256 KiB) so we get at least one intra-stream
	// ack on top of the final flush — covers both code paths.
	payload := bytes.Repeat([]byte("a"), 512*1024)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var last atomic.Int64
	cfgSend := rtc.Config{
		Logger:           logger.With("role", "send"),
		OnRemoteProgress: func(n int64) { last.Store(n) },
	}

	sendErr := make(chan error, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		sendErr <- rtc.Send(ctx, a, key, cfgSend, bytes.NewReader(payload))
	})

	var got bytes.Buffer
	recvErr := rtc.Recv(ctx, b, key, rtc.Config{Logger: logger.With("role", "recv")}, &got)
	sErr := <-sendErr
	if recvErr != nil {
		t.Fatalf("recv: %v (send err=%v)", recvErr, sErr)
	}
	if sErr != nil {
		t.Fatalf("send: %v", sErr)
	}
	// Final ack must report the full payload size; intermediate acks may be
	// smaller but the last one equals total bytes received.
	if got := last.Load(); got != int64(len(payload)) {
		t.Fatalf("last ack = %d, want %d", got, len(payload))
	}
}

func TestMismatchedKeyFailsClosed(t *testing.T) {
	t.Parallel()

	// Kept short: when keys don't match, the receiver fails its SDP
	// decrypt immediately; the sender then blocks waiting for an answer
	// that will never arrive and exits only on ctx cancellation.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	a, b := newPipeConn()
	senderKey := make([]byte, wire.SessionKeySize)
	recvKey := make([]byte, wire.SessionKeySize)
	if _, err := rand.Read(senderKey); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := rand.Read(recvKey); err != nil {
		t.Fatalf("rand: %v", err)
	}

	payload := []byte("must not be delivered")
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		_ = rtc.Send(ctx, a, senderKey, rtc.Config{Logger: logger}, bytes.NewReader(payload))
	})

	var got bytes.Buffer
	recvErr := rtc.Recv(ctx, b, recvKey, rtc.Config{Logger: logger}, &got)
	if recvErr == nil {
		t.Fatalf("recv err = nil, want failure; got=%q", got.String())
	}
	if got.Len() != 0 {
		t.Errorf("recv wrote %q despite key mismatch", got.String())
	}
}

// pipeConn is an in-memory, message-oriented wire.MsgConn pair. Each Send on
// one end becomes a single Recv on the other end. Buffered by one to avoid
// deadlock when the sender fires before the peer is ready.
type pipeConn struct {
	incoming chan []byte
	outgoing chan []byte
}

func newPipeConn() (*pipeConn, *pipeConn) {
	a2b := make(chan []byte, 4)
	b2a := make(chan []byte, 4)
	return &pipeConn{incoming: b2a, outgoing: a2b}, &pipeConn{incoming: a2b, outgoing: b2a}
}

func (p *pipeConn) Send(ctx context.Context, data []byte) error {
	buf := make([]byte, len(data))
	copy(buf, data)
	select {
	case p.outgoing <- buf:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sending on test pipe: %w", ctx.Err())
	}
}

func (p *pipeConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case m := <-p.incoming:
		return m, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("receiving on test pipe: %w", ctx.Err())
	}
}
