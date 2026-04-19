package rtc

import (
	"fmt"
	"io"
)

// The data channel framing prefixes each outbound message with a one-byte
// tag so the receiver can detect end-of-stream without relying on the
// underlying transport's close semantics. SCTP stream reset (pion native)
// propagates as io.EOF on the detached reader, but the JS RTCDataChannel
// exposes no per-direction close; wrapping both sides with a tag byte gives
// uniform behavior across platforms.
const (
	tagData byte = 0 // the remainder of the message is payload ciphertext
	tagEOF  byte = 1 // no more messages will follow; reader surfaces io.EOF
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
	scratch []byte
}

func newTagWriter(w io.Writer) *tagWriter {
	return &tagWriter{w: w, scratch: make([]byte, 0, maxFrameSize)}
}

func (t *tagWriter) Write(p []byte) (int, error) {
	if t.closed {
		return 0, fmt.Errorf("write after close")
	}
	// Reuse scratch across calls; age writes in fixed-size chunks, so the
	// buffer settles at one allocation after the first frame.
	buf := append(t.scratch[:0], tagData)
	buf = append(buf, p...)
	t.scratch = buf
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
		t.buf = append(t.buf[:0], t.msg[1:n]...)
		m := copy(p, t.buf)
		t.buf = t.buf[m:]
		return m, nil
	default:
		return 0, fmt.Errorf("unknown frame tag %d", t.msg[0])
	}
}
