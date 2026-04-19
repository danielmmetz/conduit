package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	t.Parallel()
	code := Code{Slot: 42, Words: []string{"ice", "cream", "monkey"}}
	plaintext := []byte("hello conduit — phase 3 crypto core")

	sr, rr := runExchange(t, code, code, plaintext)
	if sr.err != nil {
		t.Errorf("sender: %v", sr.err)
	}
	if rr.err != nil {
		t.Errorf("receiver: %v", rr.err)
	}
	if len(sr.key) != SessionKeySize {
		t.Errorf("sender key length = %d, want %d", len(sr.key), SessionKeySize)
	}
	if !bytes.Equal(sr.key, rr.key) {
		t.Errorf("session keys diverged: sender=%x receiver=%x", sr.key, rr.key)
	}
	if !bytes.Equal(rr.plaintext, plaintext) {
		t.Errorf("recovered plaintext = %q, want %q", rr.plaintext, plaintext)
	}
}

func TestHandshakeMismatchedCodeFailsClosed(t *testing.T) {
	t.Parallel()
	sendCode := Code{Slot: 42, Words: []string{"ice", "cream", "monkey"}}
	recvCode := Code{Slot: 42, Words: []string{"ice", "cream", "banana"}}
	plaintext := []byte("this should not decrypt")

	sr, rr := runExchange(t, sendCode, recvCode, plaintext)
	// Handshake itself completes on both sides; without a confirmation step
	// SPAKE2 doesn't detect the mismatch. Keys differ, so age scrypt unwrap
	// must reject the receiver's identity when decrypting. The sender's
	// error state isn't meaningful here — once the receiver aborts, the
	// sender will see a closed-pipe error; what matters is no plaintext
	// leaks and the keys truly differed.
	if bytes.Equal(sr.key, rr.key) {
		t.Errorf("keys unexpectedly equal under mismatched codes")
	}
	if rr.err == nil {
		t.Fatalf("receiver: expected decrypt failure, got nil; plaintext=%q", rr.plaintext)
	}
	if rr.plaintext != nil {
		t.Errorf("plaintext leaked on mismatched key: %q", rr.plaintext)
	}
}

func TestHandshakeSlotBindsKey(t *testing.T) {
	t.Parallel()
	// Same password words but different slot ids → different identity strings
	// are baked into SPAKE2, so the derived keys must diverge. This guards
	// against accidentally ignoring the slot when binding the session.
	words := []string{"ice", "cream", "monkey"}
	a := Code{Slot: 1, Words: words}
	b := Code{Slot: 2, Words: words}

	sr, rr := runExchange(t, a, b, []byte("x"))
	if bytes.Equal(sr.key, rr.key) {
		t.Errorf("keys unexpectedly equal across different slots")
	}
	if rr.err == nil {
		t.Errorf("receiver: expected decrypt failure, got nil")
	}
	if rr.plaintext != nil {
		t.Errorf("plaintext leaked across different slots: %q", rr.plaintext)
	}
}

type sendResult struct {
	key []byte
	err error
}

type recvResult struct {
	key       []byte
	plaintext []byte
	err       error
}

// runExchange drives a full sender↔receiver exchange in-process over two
// io.Pipe pairs: SPAKE2 handshake, then age-encrypted payload. Each side
// closes the pipe ends it owns on exit, so a failure on either side
// unblocks the peer instead of deadlocking. Returns once both sides have
// terminated.
func runExchange(t *testing.T, sendCode, recvCode Code, plaintext []byte) (sendResult, recvResult) {
	t.Helper()
	a2bR, a2bW := io.Pipe()
	b2aR, b2aW := io.Pipe()
	sendConn := readWriter{b2aR, a2bW}
	recvConn := readWriter{a2bR, b2aW}

	sendOut := make(chan sendResult, 1)
	recvOut := make(chan recvResult, 1)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		defer a2bW.Close()
		defer b2aR.Close()
		sendOut <- runSender(sendConn, sendCode, plaintext)
	})
	wg.Go(func() {
		defer a2bR.Close()
		defer b2aW.Close()
		recvOut <- runReceiver(recvConn, recvCode)
	})

	return <-sendOut, <-recvOut
}

func runSender(conn io.ReadWriter, code Code, plaintext []byte) sendResult {
	key, err := SendHandshake(conn, code)
	if err != nil {
		return sendResult{err: fmt.Errorf("sender handshake: %w", err)}
	}
	wc, err := Encrypt(conn, key)
	if err != nil {
		return sendResult{key: key, err: fmt.Errorf("sender encrypt: %w", err)}
	}
	if _, err := wc.Write(plaintext); err != nil {
		_ = wc.Close()
		return sendResult{key: key, err: fmt.Errorf("sender write: %w", err)}
	}
	if err := wc.Close(); err != nil {
		return sendResult{key: key, err: fmt.Errorf("sender finalize: %w", err)}
	}
	return sendResult{key: key}
}

func runReceiver(conn io.ReadWriter, code Code) recvResult {
	key, err := RecvHandshake(conn, code)
	if err != nil {
		return recvResult{err: fmt.Errorf("receiver handshake: %w", err)}
	}
	pr, err := Decrypt(conn, key)
	if err != nil {
		return recvResult{key: key, err: fmt.Errorf("receiver decrypt start: %w", err)}
	}
	got, err := io.ReadAll(pr)
	if err != nil {
		return recvResult{key: key, err: fmt.Errorf("receiver read: %w", err)}
	}
	return recvResult{key: key, plaintext: got}
}

type readWriter struct {
	io.Reader
	io.Writer
}

// sanity: make sure age.Decrypt errors really propagate and we don't silently
// treat an io.EOF as a clean "no data" read.
func TestReceiverErrorsArePropagated(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_, err := Decrypt(&buf, make([]byte, SessionKeySize))
	if err == nil {
		t.Fatalf("Decrypt(empty) = nil, want error")
	}
	if errors.Is(err, io.EOF) && strings.Contains(err.Error(), "clean") {
		t.Errorf("unexpected clean-EOF masking: %v", err)
	}
}
