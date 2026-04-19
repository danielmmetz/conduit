package wire

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/sync/errgroup"
	"salsa.debian.org/vasudev/gospake2"
)

// MsgConn is a message-oriented bidirectional transport used by the
// websocket-based PAKE path. Each Send maps to a single peer Recv.
type MsgConn interface {
	Send(ctx context.Context, data []byte) error
	Recv(ctx context.Context) ([]byte, error)
}

// SendHandshakeMsg runs the sender half of SPAKE2 over mc; see SendHandshake.
func SendHandshakeMsg(ctx context.Context, mc MsgConn, code Code) ([]byte, error) {
	return handshakeMsg(ctx, mc, code, true)
}

// RecvHandshakeMsg runs the receiver half of SPAKE2 over mc; see RecvHandshake.
func RecvHandshakeMsg(ctx context.Context, mc MsgConn, code Code) ([]byte, error) {
	return handshakeMsg(ctx, mc, code, false)
}

func handshakeMsg(ctx context.Context, mc MsgConn, code Code, sender bool) ([]byte, error) {
	return handshakeCore(code, sender, func(mine []byte) ([]byte, error) {
		var (
			eg   errgroup.Group
			peer []byte
			err  error
		)
		eg.Go(func() error { return mc.Send(ctx, mine) })
		peer, err = mc.Recv(ctx)
		if err := eg.Wait(); err != nil {
			return nil, fmt.Errorf("writing pake msg: %w", err)
		}
		if err != nil {
			return nil, fmt.Errorf("reading pake msg: %w", err)
		}
		return peer, nil
	})
}

// SessionKeySize is the length of the derived session key K in bytes.
const SessionKeySize = 32

const (
	sessionKeyInfo   = "conduit/v1/session-key"
	maxHandshakeBody = 1 << 16 // SPAKE2 messages are tens of bytes; guard against pathological input.
)

// SendHandshake runs the sender side (SPAKE2-A) of the key-agreement handshake
// over rw using code's password and slot, returning the 32-byte session key K.
// Both peers must use identical Code values; a mismatch causes Finish to fail
// and the session terminates with no key material exposed.
func SendHandshake(rw io.ReadWriter, code Code) ([]byte, error) {
	return handshake(rw, code, true)
}

// RecvHandshake runs the receiver side (SPAKE2-B); see SendHandshake.
func RecvHandshake(rw io.ReadWriter, code Code) ([]byte, error) {
	return handshake(rw, code, false)
}

func handshake(rw io.ReadWriter, code Code, sender bool) ([]byte, error) {
	return handshakeCore(code, sender, func(mine []byte) ([]byte, error) {
		// Write and read concurrently: on a synchronous transport (io.Pipe
		// in tests, or any unbuffered conn) a write-then-read from both
		// sides deadlocks. errgroup.Wait explicitly joins the write
		// goroutine before we return on every path.
		var (
			eg   errgroup.Group
			peer []byte
			err  error
		)
		eg.Go(func() error { return writeFrame(rw, mine) })
		peer, err = readFrame(rw)
		if err := eg.Wait(); err != nil {
			return nil, fmt.Errorf("writing pake msg: %w", err)
		}
		if err != nil {
			return nil, fmt.Errorf("reading pake msg: %w", err)
		}
		return peer, nil
	})
}

func handshakeCore(code Code, sender bool, exchange func(mine []byte) ([]byte, error)) ([]byte, error) {
	pw := gospake2.NewPassword(code.Password())
	idA := gospake2.NewIdentityA(fmt.Sprintf("conduit/v1/slot/%d/A", code.Slot))
	idB := gospake2.NewIdentityB(fmt.Sprintf("conduit/v1/slot/%d/B", code.Slot))
	var s gospake2.SPAKE2
	if sender {
		s = gospake2.SPAKE2A(pw, idA, idB)
	} else {
		s = gospake2.SPAKE2B(pw, idA, idB)
	}
	peer, err := exchange(s.Start())
	if err != nil {
		return nil, fmt.Errorf("exchanging pake messages: %w", err)
	}
	shared, err := s.Finish(peer)
	if err != nil {
		return nil, fmt.Errorf("finishing pake: %w", err)
	}
	key, err := hkdf.Key(sha256.New, shared, nil, sessionKeyInfo, SessionKeySize)
	if err != nil {
		return nil, fmt.Errorf("deriving session key: %w", err)
	}
	return key, nil
}

func writeFrame(w io.Writer, body []byte) error {
	if len(body) > maxHandshakeBody {
		return fmt.Errorf("handshake frame too large: %d bytes", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("writing frame header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("writing frame body: %w", err)
	}
	return nil
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("reading frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxHandshakeBody {
		return nil, fmt.Errorf("frame too large: %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("reading frame body: %w", err)
	}
	return body, nil
}
