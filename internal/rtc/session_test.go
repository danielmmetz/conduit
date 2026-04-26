package rtc_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/rtc"
	"github.com/danielmmetz/conduit/internal/wire"
)

// TestSessionRoundTrip establishes a session and runs one transfer in one
// direction, mirroring the v1 TestSendRecvRoundTrip but going through the
// new Session API.
func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := newKey(t)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	payload := bytes.Repeat([]byte("session round trip — single transfer\n"), 256)

	sessA, sessB := openPair(t, ctx, a, b, key, logger)
	defer closePair(t, ctx, sessA, sessB)

	pullErr := make(chan error, 1)
	var got bytes.Buffer
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		pullErr <- sessB.Pull(ctx, &got)
	})

	if err := sessA.Push(ctx, bytes.NewReader(payload)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := <-pullErr; err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", got.Len(), len(payload))
	}
}

// TestSessionMultiTransfer pushes three separate transfers in sequence
// over one session, verifying the channel is genuinely persistent and
// the receiver's Pull / sender's Push compose without per-transfer
// teardown.
func TestSessionMultiTransfer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := newKey(t)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	sessA, sessB := openPair(t, ctx, a, b, key, logger)
	defer closePair(t, ctx, sessA, sessB)

	transfers := [][]byte{
		[]byte("first transfer"),
		bytes.Repeat([]byte("second transfer payload, somewhat larger\n"), 64),
		[]byte("third"),
	}

	for i, want := range transfers {
		pullErr := make(chan error, 1)
		var got bytes.Buffer
		var wg sync.WaitGroup
		wg.Go(func() {
			pullErr <- sessB.Pull(ctx, &got)
		})
		if err := sessA.Push(ctx, bytes.NewReader(want)); err != nil {
			t.Fatalf("transfer %d push: %v", i, err)
		}
		if err := <-pullErr; err != nil {
			t.Fatalf("transfer %d pull: %v", i, err)
		}
		wg.Wait()
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("transfer %d: got %q, want %q", i, got.String(), want)
		}
	}
}

// TestSessionConcurrentBidirectional pushes from both peers simultaneously
// — the inbound and outbound DCs are independent, so opposite-direction
// transfers must not block each other.
func TestSessionConcurrentBidirectional(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := newKey(t)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	sessA, sessB := openPair(t, ctx, a, b, key, logger)
	defer closePair(t, ctx, sessA, sessB)

	// Larger payloads here — 512 KiB each — to ensure the transfers really
	// overlap in time rather than each completing in a single SCTP message.
	payloadA := bytes.Repeat([]byte("A"), 512*1024)
	payloadB := bytes.Repeat([]byte("B"), 512*1024)

	var wg sync.WaitGroup
	pushAErr := make(chan error, 1)
	pushBErr := make(chan error, 1)
	pullAErr := make(chan error, 1)
	pullBErr := make(chan error, 1)
	var gotAtoB, gotBtoA bytes.Buffer

	wg.Go(func() { pushAErr <- sessA.Push(ctx, bytes.NewReader(payloadA)) })
	wg.Go(func() { pushBErr <- sessB.Push(ctx, bytes.NewReader(payloadB)) })
	wg.Go(func() { pullAErr <- sessB.Pull(ctx, &gotAtoB) })
	wg.Go(func() { pullBErr <- sessA.Pull(ctx, &gotBtoA) })
	wg.Wait()

	if err := <-pushAErr; err != nil {
		t.Fatalf("push A: %v", err)
	}
	if err := <-pushBErr; err != nil {
		t.Fatalf("push B: %v", err)
	}
	if err := <-pullAErr; err != nil {
		t.Fatalf("pull A→B: %v", err)
	}
	if err := <-pullBErr; err != nil {
		t.Fatalf("pull B→A: %v", err)
	}
	if !bytes.Equal(gotAtoB.Bytes(), payloadA) {
		t.Fatalf("A→B payload mismatch: got %d, want %d", gotAtoB.Len(), len(payloadA))
	}
	if !bytes.Equal(gotBtoA.Bytes(), payloadB) {
		t.Fatalf("B→A payload mismatch: got %d, want %d", gotBtoA.Len(), len(payloadB))
	}
}

// TestSessionAckProgress confirms the demuxer surfaces tagAck values
// through cfg.OnRemoteProgress on the sending peer.
func TestSessionAckProgress(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	a, b := newPipeConn()
	key := newKey(t)
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	// Larger than the 256 KiB ack threshold so we get at least one
	// intra-stream ack on top of the final flush — covers both code paths.
	payload := bytes.Repeat([]byte("a"), 512*1024)

	var lastAck atomic.Int64
	cfgA := rtc.Config{
		Logger:           logger.With("role", "A"),
		OnRemoteProgress: func(n int64) { lastAck.Store(n) },
	}
	cfgB := rtc.Config{Logger: logger.With("role", "B")}

	type opened struct {
		sess *rtc.Session
		err  error
	}
	aRes := make(chan opened, 1)
	bRes := make(chan opened, 1)
	go func() {
		s, err := rtc.Initiate(ctx, a, key, cfgA)
		aRes <- opened{s, err}
	}()
	go func() {
		s, err := rtc.Respond(ctx, b, key, cfgB)
		bRes <- opened{s, err}
	}()
	ar := <-aRes
	if ar.err != nil {
		t.Fatalf("initiate: %v", ar.err)
	}
	br := <-bRes
	if br.err != nil {
		t.Fatalf("respond: %v", br.err)
	}
	sessA, sessB := ar.sess, br.sess
	defer closePair(t, ctx, sessA, sessB)

	pullErr := make(chan error, 1)
	var got bytes.Buffer
	go func() { pullErr <- sessB.Pull(ctx, &got) }()
	if err := sessA.Push(ctx, bytes.NewReader(payload)); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := <-pullErr; err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Acks may still be in flight after Pull returns (they're fired from
	// the demuxer goroutine on the sender's inbound DC). Close drains them
	// before tearing down. After closePair, lastAck should equal the
	// payload size.
	// closePair below in the deferred call performs the wait; we need to
	// observe lastAck before close, so wait briefly with a deadline.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastAck.Load() == int64(len(payload)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("last ack = %d, want %d", lastAck.Load(), len(payload))
}

// openPair starts Initiate on a and Respond on b concurrently and
// returns the established Sessions. Fails the test on any open error.
func openPair(t *testing.T, ctx context.Context, a, b wire.MsgConn, key []byte, logger *slog.Logger) (*rtc.Session, *rtc.Session) {
	t.Helper()
	type opened struct {
		sess *rtc.Session
		err  error
	}
	aRes := make(chan opened, 1)
	bRes := make(chan opened, 1)
	go func() {
		s, err := rtc.Initiate(ctx, a, key, rtc.Config{Logger: logger.With("role", "A")})
		aRes <- opened{s, err}
	}()
	go func() {
		s, err := rtc.Respond(ctx, b, key, rtc.Config{Logger: logger.With("role", "B")})
		bRes <- opened{s, err}
	}()
	ar := <-aRes
	br := <-bRes
	if ar.err != nil {
		t.Fatalf("initiate: %v", ar.err)
	}
	if br.err != nil {
		t.Fatalf("respond: %v", br.err)
	}
	return ar.sess, br.sess
}

// closePair closes both sessions in parallel — the teardown handshake
// requires both sides to send and receive, so closing them serially
// would deadlock.
func closePair(t *testing.T, ctx context.Context, a, b *rtc.Session) {
	t.Helper()
	var wg sync.WaitGroup
	var aErr, bErr error
	wg.Go(func() { aErr = a.Close(ctx) })
	wg.Go(func() { bErr = b.Close(ctx) })
	wg.Wait()
	if aErr != nil {
		t.Errorf("close A: %v", aErr)
	}
	if bErr != nil {
		t.Errorf("close B: %v", bErr)
	}
}

func newKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, wire.SessionKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return key
}
