package rtc

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// The data channel framing prefixes each outbound message with a one-byte
// tag so the receiver can detect end-of-stream without relying on the
// underlying transport's close semantics. SCTP stream reset (pion native)
// propagates as io.EOF on the detached reader, but the JS RTCDataChannel
// exposes no per-direction close; wrapping both sides with a tag byte gives
// uniform behavior across platforms.
//
// Frames travel both directions: sender → receiver carries tagData /
// tagEOF (payload ciphertext); receiver → sender carries tagAck (progress
// reports). Because only one peer writes each tag class, the two directions
// do not collide on a shared data channel.
const (
	tagData byte = 0 // sender → receiver: the remainder is payload ciphertext
	tagEOF  byte = 1 // sender → receiver: no more payload frames will follow
	tagAck  byte = 2 // receiver → sender: cumulative plaintext bytes received
)

// maxFrameSize bounds the receive buffer for a single DC message. Age's
// STREAM chunks are 64 KiB plus ~16 bytes of overhead; a tag byte and a bit
// of slack keep this well under the 256 KiB default browser max-message-size.
const maxFrameSize = 128 * 1024

// tagWriter wraps a message-oriented data-channel writer. Each Write emits
// exactly one DC message prefixed with tagData; Close emits a single tagEOF
// sentinel message so the peer can terminate its read loop cleanly.
type tagWriter struct {
	w      io.Writer
	closed bool
}

func newTagWriter(w io.Writer) *tagWriter {
	return &tagWriter{w: w}
}

func (t *tagWriter) Write(p []byte) (int, error) {
	if t.closed {
		return 0, fmt.Errorf("write after close")
	}
	buf := make([]byte, 1+len(p))
	buf[0] = tagData
	copy(buf[1:], p)
	if _, err := t.w.Write(buf); err != nil {
		return 0, fmt.Errorf("writing data frame: %w", err)
	}
	return len(p), nil
}

func (t *tagWriter) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	if _, err := t.w.Write([]byte{tagEOF}); err != nil {
		return fmt.Errorf("writing eof frame: %w", err)
	}
	return nil
}

// tagReader wraps a message-oriented data-channel reader. Each underlying
// Read is expected to yield exactly one tagged message; tagReader returns the
// payload bytes across one or more caller Read calls and surfaces io.EOF when
// it observes the tagEOF sentinel.
type tagReader struct {
	r   io.Reader
	buf []byte
	eof bool
	msg []byte
}

func newTagReader(r io.Reader) *tagReader {
	return &tagReader{r: r, msg: make([]byte, maxFrameSize)}
}

func (t *tagReader) Read(p []byte) (int, error) {
	if len(t.buf) > 0 {
		n := copy(p, t.buf)
		t.buf = t.buf[n:]
		return n, nil
	}
	if t.eof {
		return 0, io.EOF
	}
	n, err := t.r.Read(t.msg)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("empty frame")
	}
	switch t.msg[0] {
	case tagEOF:
		t.eof = true
		return 0, io.EOF
	case tagData:
		if n == 1 {
			return 0, fmt.Errorf("empty data frame")
		}
		t.buf = append(t.buf[:0], t.msg[1:n]...)
		m := copy(p, t.buf)
		t.buf = t.buf[m:]
		return m, nil
	default:
		return 0, fmt.Errorf("unknown frame tag %d", t.msg[0])
	}
}

// ackFrameSize is the on-wire size of a tagAck frame: one tag byte + int64
// big-endian total. Fits in a single data-channel message.
const ackFrameSize = 1 + 8

// writeAck emits a single tagAck data-channel message carrying the cumulative
// plaintext byte count received so far. Called by the recv side.
func writeAck(w io.Writer, total int64) error {
	var buf [ackFrameSize]byte
	buf[0] = tagAck
	binary.BigEndian.PutUint64(buf[1:], uint64(total))
	if _, err := w.Write(buf[:]); err != nil {
		return fmt.Errorf("writing ack frame: %w", err)
	}
	return nil
}

// readAcks loops reading tagged messages from r; for each tagAck frame it
// invokes cb with the cumulative total. Returns when r.Read returns any
// error (io.EOF / io.ErrClosedPipe on clean shutdown, or a real failure) or
// when ctx is cancelled — in which case a watcher goroutine invokes unblock
// to break r.Read out of its blocking syscall, and the returned error wraps
// ctx.Err(). unblock must be safe to call multiple times: the watcher always
// fires once readAcks returns, so callers that also close r in their own
// defer should pass an idempotent (e.g. sync.OnceFunc) closer.
//
// Unknown tags are tolerated silently for forward compat; the sender only
// ever expects tagAck over the receiver→sender direction, but a future peer
// could layer new control frames without breaking older senders.
func readAcks(ctx context.Context, r io.Reader, unblock func(), cb func(int64)) error {
	ctx, cancel := context.WithCancel(ctx)
	// Watcher forces r.Read to unblock when ctx fires. The derived ctx is
	// also cancelled when readAcks returns naturally (via the defer below),
	// so the watcher always terminates.
	var wg sync.WaitGroup
	wg.Go(func() {
		<-ctx.Done()
		unblock()
	})
	// cancel before wg.Wait so the watcher unblocks and can be joined.
	defer func() {
		cancel()
		wg.Wait()
	}()

	msg := make([]byte, maxFrameSize)
	for {
		n, err := r.Read(msg)
		if err != nil {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("reading ack frame: %w", err)
			}
			return err
		}
		if n == 0 {
			return fmt.Errorf("reading ack frame: empty message")
		}
		if msg[0] != tagAck {
			continue
		}
		if n != ackFrameSize {
			return fmt.Errorf("reading ack frame: got %d bytes, want %d", n, ackFrameSize)
		}
		total := int64(binary.BigEndian.Uint64(msg[1:n]))
		if cb != nil {
			cb(total)
		}
	}
}

// ackingWriter wraps the receiver's sink with a counter; every ackThreshold
// plaintext bytes written, it emits a tagAck frame back to raw so the sender
// can surface peer-side progress. A final ack is emitted on Flush().
type ackingWriter struct {
	dst          io.Writer
	raw          io.Writer
	written      int64
	sinceLastAck int64
	ackThreshold int64
}

// defaultAckThreshold bounds ack frequency. At 256 KiB/ack and a 64 KiB age
// chunk size, one ack per ~4 payload chunks — enough resolution for a
// progress bar without flooding the reverse channel on fast local transfers.
const defaultAckThreshold = 256 * 1024

func newAckingWriter(dst, raw io.Writer) *ackingWriter {
	return &ackingWriter{dst: dst, raw: raw, ackThreshold: defaultAckThreshold}
}

func (a *ackingWriter) Write(p []byte) (int, error) {
	n, err := a.dst.Write(p)
	if n > 0 {
		a.written += int64(n)
		a.sinceLastAck += int64(n)
		if a.sinceLastAck >= a.ackThreshold {
			if aerr := writeAck(a.raw, a.written); aerr != nil {
				return n, fmt.Errorf("emitting ack: %w", aerr)
			}
			a.sinceLastAck = 0
		}
	}
	return n, err
}

// Flush emits a final ack with the cumulative count so the sender sees an
// accurate final total even when the stream ends inside an ack window.
// Best-effort; any error is wrapped and returned.
func (a *ackingWriter) Flush() error {
	if a.written == 0 {
		return nil
	}
	if err := writeAck(a.raw, a.written); err != nil {
		return fmt.Errorf("flushing final ack: %w", err)
	}
	a.sinceLastAck = 0
	return nil
}
