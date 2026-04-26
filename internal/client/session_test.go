package client_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/danielmmetz/conduit/internal/wire"
)

// TestSessionRoundTrip drives a full end-to-end session against a real
// signaling server: sender and receiver each open a Session, the sender
// pushes one transfer, the receiver's SinkOpener delivers the payload, and
// both peers close cleanly.
func TestSessionRoundTrip(t *testing.T) {
	t.Parallel()

	ts := newSignalingTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	codeCh := make(chan string, 1)
	type opened struct {
		sess *client.Session
		err  error
	}
	senderRes := make(chan opened, 1)
	receiverRes := make(chan opened, 1)
	go func() {
		s, err := client.OpenSender(ctx, logger.With("role", "sender"), ts.URL, client.RelayAuto, func(code string) { codeCh <- code }, nil)
		senderRes <- opened{s, err}
	}()
	code := <-codeCh
	parsed, err := wire.ParseCode(code)
	if err != nil {
		t.Fatalf("parse code: %v", err)
	}

	var got bytes.Buffer
	var gotPreamble wire.Preamble
	transferDone := make(chan struct{})
	openSink := func(p wire.Preamble) (io.WriteCloser, error) {
		gotPreamble = p
		return finalizeCloser{
			Writer: &got,
			onClose: func() error {
				close(transferDone)
				return nil
			},
		}, nil
	}
	go func() {
		s, err := client.OpenReceiver(ctx, logger.With("role", "receiver"), ts.URL, parsed, client.RelayAuto, openSink)
		receiverRes <- opened{s, err}
	}()

	sr := <-senderRes
	if sr.err != nil {
		t.Fatalf("OpenSender: %v", sr.err)
	}
	rr := <-receiverRes
	if rr.err != nil {
		t.Fatalf("OpenReceiver: %v", rr.err)
	}
	sender, receiver := sr.sess, rr.sess

	payload := []byte("session round trip via real signaling")
	preamble := wire.Preamble{Kind: wire.PreambleKindText, Name: "msg", Size: int64(len(payload))}
	if err := sender.Push(ctx, preamble, bytes.NewReader(payload)); err != nil {
		t.Fatalf("push: %v", err)
	}
	select {
	case <-transferDone:
	case <-ctx.Done():
		t.Fatalf("transfer not received before deadline")
	}

	closePair(t, ctx, sender, receiver)

	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("payload mismatch: got %q, want %q", got.String(), payload)
	}
	if gotPreamble.Name != preamble.Name {
		t.Errorf("preamble name = %q, want %q", gotPreamble.Name, preamble.Name)
	}
}

// TestSessionMultiPushSameDirection verifies a session can carry several
// sender-to-receiver transfers without re-pairing.
func TestSessionMultiPushSameDirection(t *testing.T) {
	t.Parallel()

	ts := newSignalingTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))

	type received struct {
		preamble wire.Preamble
		body     []byte
	}
	var recvMu sync.Mutex
	var recvList []received
	transferDone := make(chan int, 8)

	openSink := func(p wire.Preamble) (io.WriteCloser, error) {
		var buf bytes.Buffer
		return finalizeCloser{
			Writer: &buf,
			onClose: func() error {
				recvMu.Lock()
				recvList = append(recvList, received{preamble: p, body: append([]byte(nil), buf.Bytes()...)})
				count := len(recvList)
				recvMu.Unlock()
				transferDone <- count
				return nil
			},
		}, nil
	}

	codeCh := make(chan string, 1)
	type opened struct {
		sess *client.Session
		err  error
	}
	senderRes := make(chan opened, 1)
	receiverRes := make(chan opened, 1)
	go func() {
		s, err := client.OpenSender(ctx, logger.With("role", "sender"), ts.URL, client.RelayAuto, func(code string) { codeCh <- code }, nil)
		senderRes <- opened{s, err}
	}()
	code := <-codeCh
	parsed, err := wire.ParseCode(code)
	if err != nil {
		t.Fatalf("parse code: %v", err)
	}
	go func() {
		s, err := client.OpenReceiver(ctx, logger.With("role", "receiver"), ts.URL, parsed, client.RelayAuto, openSink)
		receiverRes <- opened{s, err}
	}()
	sr := <-senderRes
	if sr.err != nil {
		t.Fatalf("OpenSender: %v", sr.err)
	}
	rr := <-receiverRes
	if rr.err != nil {
		t.Fatalf("OpenReceiver: %v", rr.err)
	}
	sender, receiver := sr.sess, rr.sess

	want := []struct {
		name string
		body []byte
	}{
		{"first", []byte("alpha")},
		{"second", bytes.Repeat([]byte("beta payload "), 50)},
		{"third", []byte("γ")},
	}
	for _, w := range want {
		preamble := wire.Preamble{Kind: wire.PreambleKindText, Name: w.name, Size: int64(len(w.body))}
		if err := sender.Push(ctx, preamble, bytes.NewReader(w.body)); err != nil {
			t.Fatalf("push %q: %v", w.name, err)
		}
	}
	for i := range want {
		select {
		case <-transferDone:
		case <-ctx.Done():
			t.Fatalf("only %d/%d transfers received before deadline", i, len(want))
		}
	}

	closePair(t, ctx, sender, receiver)

	recvMu.Lock()
	defer recvMu.Unlock()
	if len(recvList) != len(want) {
		t.Fatalf("received %d transfers, want %d", len(recvList), len(want))
	}
	for i, w := range want {
		if recvList[i].preamble.Name != w.name {
			t.Errorf("transfer %d name = %q, want %q", i, recvList[i].preamble.Name, w.name)
		}
		if !bytes.Equal(recvList[i].body, w.body) {
			t.Errorf("transfer %d body mismatch", i)
		}
	}
}

func newSignalingTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	srv := signaling.NewServer(logger,
		signaling.WithSlotTTL(10*time.Second),
		signaling.WithHelloTimeout(5*time.Second),
	)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	mux.HandleFunc("GET /healthz", srv.HandleHealthz)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// closePair closes both sessions in parallel — exchangeTeardown is bidirectional,
// so closing them serially would deadlock.
func closePair(t *testing.T, ctx context.Context, a, b *client.Session) {
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

type nopCloser struct {
	w io.Writer
}

func (n nopCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n nopCloser) Close() error                { return nil }

type finalizeCloser struct {
	io.Writer
	onClose func() error
}

func (f finalizeCloser) Close() error {
	if f.onClose != nil {
		return f.onClose()
	}
	return nil
}

var _ = fmt.Sprint
