package xfer

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/sync/errgroup"
)

// CompressMode is the user-facing knob from --compress.
type CompressMode int

const (
	// CompressAuto skips the codec for already-compressed MIME types and
	// for tiny text payloads, and enables zstd otherwise. The default.
	CompressAuto CompressMode = iota
	// CompressZstd forces the zstd codec on regardless of payload shape.
	CompressZstd
	// CompressNone forces no compression on regardless of payload shape.
	CompressNone
)

// alreadyCompressedMIME reports MIME types where running zstd on the payload
// would burn CPU for ~zero gain. The list is conservative: SVG is left out
// because it's text under image/* and benefits like other text. Format
// guesses come from the sender's MIME detection in source.go.
func alreadyCompressedMIME(m string) bool {
	m = strings.ToLower(m)
	if strings.HasPrefix(m, "image/") && !strings.HasPrefix(m, "image/svg") {
		return true
	}
	if strings.HasPrefix(m, "video/") || strings.HasPrefix(m, "audio/") {
		return true
	}
	switch {
	case strings.HasPrefix(m, "application/zip"),
		strings.HasPrefix(m, "application/gzip"),
		strings.HasPrefix(m, "application/x-7z-compressed"),
		strings.HasPrefix(m, "application/x-xz"),
		strings.HasPrefix(m, "application/x-zstd"),
		strings.HasPrefix(m, "application/x-rar-compressed"),
		strings.HasPrefix(m, "application/vnd.rar"),
		strings.HasPrefix(m, "application/x-bzip2"):
		return true
	}
	return false
}

// pickCompression decides which codec to advertise on the preamble given the
// payload shape and the user's mode. Tar streams almost always benefit; text
// is too small to pay back the codec overhead; single files defer to MIME.
// Stdin and unknown-extension binaries fall through with MIME=
// application/octet-stream (not in the already-compressed list) and so go to
// zstd in auto mode — we can't peek at the bytes to decide otherwise.
func pickCompression(p wire.Preamble, mode CompressMode) string {
	switch mode {
	case CompressNone:
		return wire.PreambleCompressionNone
	case CompressZstd:
		return wire.PreambleCompressionZstd
	}
	switch p.Kind {
	case wire.PreambleKindText:
		return wire.PreambleCompressionNone
	case wire.PreambleKindTar:
		return wire.PreambleCompressionZstd
	case wire.PreambleKindFile:
		if alreadyCompressedMIME(p.MIME) {
			return wire.PreambleCompressionNone
		}
		return wire.PreambleCompressionZstd
	}
	return wire.PreambleCompressionNone
}

// encodeSource is the read side of an in-process zstd encoder. Reads pull
// compressed bytes; the producer goroutine pulls user bytes from the inner
// source and feeds the encoder. Close is the only join point for the
// producer goroutine — callers must always Close even on error paths.
type encodeSource struct {
	pr  *io.PipeReader
	src io.Closer
	eg  *errgroup.Group
}

func (e *encodeSource) Read(p []byte) (int, error) { return e.pr.Read(p) }

// Close joins the encoder goroutine. Both unblock paths must run before
// eg.Wait: closing pr unblocks the encoder if it's stuck on pw.Write
// (consumer stopped reading), and closing src unblocks it if it's stuck on
// a source-side Read (e.g. a pipe with no producer). Calling either one in
// isolation can leave the other branch blocked, so we issue both and only
// then wait.
func (e *encodeSource) Close() error {
	var errs []error
	if err := e.pr.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing encode pipe reader: %w", err))
	}
	if err := e.src.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing encode source: %w", err))
	}
	if err := e.eg.Wait(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// wrapEncode wraps src so reads return the zstd-compressed stream. The
// returned ReadCloser takes ownership of src — the caller must not Close it
// directly. For codec == none the source is returned unchanged.
func wrapEncode(src io.ReadCloser, codec string) (io.ReadCloser, error) {
	if codec == wire.PreambleCompressionNone {
		return src, nil
	}
	if codec != wire.PreambleCompressionZstd {
		_ = src.Close()
		return nil, fmt.Errorf("wrapping encoder: unsupported codec %q", codec)
	}
	pr, pw := io.Pipe()
	enc, err := zstd.NewWriter(pw,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}
	var eg errgroup.Group
	eg.Go(func() error {
		_, copyErr := io.Copy(enc, src)
		encErr := enc.Close()
		if joined := errors.Join(copyErr, encErr); joined != nil {
			_ = pw.CloseWithError(joined)
			return fmt.Errorf("compressing source: %w", joined)
		}
		if err := pw.Close(); err != nil {
			return fmt.Errorf("closing pipe writer: %w", err)
		}
		return nil
	})
	return &encodeSource{pr: pr, src: src, eg: &eg}, nil
}

// decodeSink is the write side of an in-process zstd decoder. Writes push
// compressed bytes into a pipe whose reader feeds the decoder; the decoder
// goroutine writes plaintext into the inner sink. Close is the only join
// point for the decoder goroutine — callers must always Close.
type decodeSink struct {
	pw   *io.PipeWriter
	sink io.WriteCloser
	eg   *errgroup.Group
}

func (d *decodeSink) Write(p []byte) (int, error) {
	n, err := d.pw.Write(p)
	if err != nil {
		return n, fmt.Errorf("feeding zstd decoder: %w", err)
	}
	return n, nil
}

// Close drains the decoder goroutine and then closes the inner sink.
// Order: closing pw signals EOF to the decoder loop; eg.Wait joins it; then
// we release the inner sink we took ownership of in WrapDecode.
func (d *decodeSink) Close() error {
	var errs []error
	if err := d.pw.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing decode pipe writer: %w", err))
	}
	if err := d.eg.Wait(); err != nil {
		errs = append(errs, err)
	}
	if err := d.sink.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing decode sink: %w", err))
	}
	return errors.Join(errs...)
}

// WrapDecode wraps sink so writes are zstd-decoded before reaching it. On
// success the returned WriteCloser takes ownership of sink — the caller must
// not Close it directly. On error sink is NOT closed; ownership stays with
// the caller. This matters for sinks whose Close has user-visible side
// effects (e.g. the WASM bridge's onEnd callback) — closing them on a setup
// failure would emit a spurious "transfer complete" event before the error
// propagates. For codec == none the sink is returned unchanged.
func WrapDecode(sink io.WriteCloser, codec string) (io.WriteCloser, error) {
	if codec == wire.PreambleCompressionNone {
		return sink, nil
	}
	if codec != wire.PreambleCompressionZstd {
		return nil, fmt.Errorf("wrapping decoder: unsupported codec %q", codec)
	}
	pr, pw := io.Pipe()
	dec, err := zstd.NewReader(pr)
	if err != nil {
		return nil, fmt.Errorf("creating zstd decoder: %w", err)
	}
	var eg errgroup.Group
	eg.Go(func() error {
		_, copyErr := io.Copy(sink, dec)
		dec.Close()
		if copyErr != nil {
			_ = pr.CloseWithError(copyErr)
			return fmt.Errorf("decompressing payload: %w", copyErr)
		}
		return nil
	})
	return &decodeSink{pw: pw, sink: sink, eg: &eg}, nil
}

// countingWriter wraps an io.WriteCloser, invoking cb with the cumulative
// byte count after each successful Write. Used inside OpenSink so receiver
// progress reflects post-decompression (user-visible) bytes.
type countingWriter struct {
	w  io.WriteCloser
	cb func(int64)
	n  int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.n += int64(n)
		if c.cb != nil {
			c.cb(c.n)
		}
	}
	if err != nil {
		return n, fmt.Errorf("counting writer: %w", err)
	}
	return n, nil
}

func (c *countingWriter) Close() error {
	if err := c.w.Close(); err != nil {
		return fmt.Errorf("closing counting writer: %w", err)
	}
	return nil
}
