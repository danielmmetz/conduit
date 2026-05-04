package xfer_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/danielmmetz/conduit/internal/xfer"
	"github.com/klauspost/compress/zstd"
)

// trackingSink is an in-memory io.WriteCloser that records whether Close
// was called, mirroring the WASM bridge's wasmTransferSink: a sink whose
// Close has user-visible side effects (firing onEnd). The xfer-side tests
// use it to verify WrapDecode's ownership contract directly.
type trackingSink struct {
	buf    bytes.Buffer
	closed bool
}

func (t *trackingSink) Write(p []byte) (int, error) { return t.buf.Write(p) }
func (t *trackingSink) Close() error {
	t.closed = true
	return nil
}

// TestOpenPathsAutoCompressesText: a text/plain file in auto mode must end up
// with zstd compression on the preamble, and the wire byte count should be
// strictly smaller than the original for highly compressible content.
func TestOpenPathsAutoCompressesText(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "compressible.txt")
	payload := bytes.Repeat([]byte("aaaaaaaaaa"), 4096) // 40 KiB of 'a'
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressAuto})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Preamble.Compression; got != wire.PreambleCompressionZstd {
		t.Fatalf("Compression = %q, want %q", got, wire.PreambleCompressionZstd)
	}
	if s.Preamble.Size != int64(len(payload)) {
		t.Fatalf("Size = %d, want %d (uncompressed user bytes)", s.Preamble.Size, len(payload))
	}
	encoded, err := io.ReadAll(s.Reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if int64(len(encoded)) >= int64(len(payload)) {
		t.Fatalf("compressed wire size %d is not smaller than payload %d", len(encoded), len(payload))
	}
}

// TestOpenPathsAutoSkipsAlreadyCompressedMIME: a .jpg in auto mode must
// resolve to none; running zstd on a JPEG would be CPU-for-nothing.
func TestOpenPathsAutoSkipsAlreadyCompressedMIME(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "photo.jpg")
	if err := os.WriteFile(path, []byte("not really a jpg, but the MIME table will route us"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressAuto})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Preamble.Compression; got != wire.PreambleCompressionNone {
		t.Fatalf("Compression = %q, want %q (auto must skip image/jpeg)", got, wire.PreambleCompressionNone)
	}
}

// TestOpenPathsForceNone: --compress=none disables zstd even for content
// where auto would have enabled it.
func TestOpenPathsForceNone(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "compressible.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 1024), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressNone})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Preamble.Compression; got != wire.PreambleCompressionNone {
		t.Fatalf("Compression = %q, want %q", got, wire.PreambleCompressionNone)
	}
}

// TestOpenPathsForceZstd: --compress=zstd forces compression even for a
// MIME the auto policy would have skipped.
func TestOpenPathsForceZstd(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "photo.jpg")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 1024), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressZstd})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Preamble.Compression; got != wire.PreambleCompressionZstd {
		t.Fatalf("Compression = %q, want %q", got, wire.PreambleCompressionZstd)
	}
}

// TestOpenTextAlwaysNone: text payloads in auto mode should never be
// compressed (header overhead exceeds the gain at typical sizes).
func TestOpenTextAlwaysNone(t *testing.T) {
	t.Parallel()
	s, err := xfer.OpenText("hello world", xfer.SourceOptions{Compression: xfer.CompressAuto})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if got := s.Preamble.Compression; got != wire.PreambleCompressionNone {
		t.Fatalf("Compression = %q, want %q", got, wire.PreambleCompressionNone)
	}
}

// TestRoundTripFileWithZstd: source produces compressed bytes, OpenSink
// decodes them, and the file on disk matches the original payload exactly.
func TestRoundTripFileWithZstd(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "in.bin")
	payload := []byte("conduit zstd round-trip — needs to come back identical\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressZstd})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Compression != wire.PreambleCompressionZstd {
		t.Fatalf("Compression = %q, want zstd", s.Preamble.Compression)
	}

	out := t.TempDir()
	sink, err := xfer.OpenSink(s.Preamble, xfer.SinkOptions{OutPath: filepath.Join(out, "in.bin")})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, s.Reader); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(out, "in.bin"))
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload differs after round-trip:\n  got  %q\n  want %q", got, payload)
	}
}

// TestRoundTripTarWithZstd: same idea but for the tar source path —
// directory contents survive the encode/decode pipeline.
func TestRoundTripTarWithZstd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), strings.Repeat("alpha\n", 200))
	mustWrite(t, filepath.Join(root, "nested", "b.txt"), strings.Repeat("bravo\n", 200))

	s, err := xfer.OpenPaths([]string{root}, nil, false, xfer.SourceOptions{Compression: xfer.CompressZstd})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Compression != wire.PreambleCompressionZstd {
		t.Fatalf("Compression = %q, want zstd", s.Preamble.Compression)
	}

	outRoot := t.TempDir()
	sink, err := xfer.OpenSink(s.Preamble, xfer.SinkOptions{OutPath: outRoot})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, s.Reader); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}
	base := filepath.Base(root)
	checkFile(t, filepath.Join(outRoot, base, "a.txt"), strings.Repeat("alpha\n", 200))
	checkFile(t, filepath.Join(outRoot, base, "nested", "b.txt"), strings.Repeat("bravo\n", 200))
}

// TestSourceProgressCallsCountUncompressed: the SourceOptions.Progress
// callback fires with cumulative uncompressed byte counts even when zstd
// is on, and the final tally equals the user payload size.
func TestSourceProgressCallsCountUncompressed(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "in.txt")
	payload := bytes.Repeat([]byte("a"), 8192)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var lastN atomic.Int64
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{
		Compression: xfer.CompressZstd,
		Progress:    func(n int64) { lastN.Store(n) },
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if _, err := io.Copy(io.Discard, s.Reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := lastN.Load(); got != int64(len(payload)) {
		t.Fatalf("Progress final = %d, want %d (uncompressed bytes)", got, len(payload))
	}
}

// TestSinkProgressCallsCountUncompressed: SinkOptions.Progress fires with
// post-decompression byte counts so the receiver always sees user-visible
// numbers regardless of the wire codec.
func TestSinkProgressCallsCountUncompressed(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "in.bin")
	payload := bytes.Repeat([]byte("z"), 16384)
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	src, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressZstd})
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()

	var lastN atomic.Int64
	out := t.TempDir()
	sink, err := xfer.OpenSink(src.Preamble, xfer.SinkOptions{
		OutPath:  filepath.Join(out, "in.bin"),
		Progress: func(n int64) { lastN.Store(n) },
	})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, src.Reader); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}
	if got := lastN.Load(); got != int64(len(payload)) {
		t.Fatalf("Progress final = %d, want %d (uncompressed bytes)", got, len(payload))
	}
}

// TestSourceCloseAfterPartialRead verifies the encoder goroutine joins
// cleanly when the consumer stops reading before EOF. With a payload
// larger than the pipe buffer, the producer is guaranteed to be in a
// pw.Write or src.Read at the moment Close runs. A fail-after timer
// guards against regressions that would otherwise hang the test.
func TestSourceCloseAfterPartialRead(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "data.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), 256*1024), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false, xfer.SourceOptions{Compression: xfer.CompressZstd})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, 32)
	if _, err := s.Reader.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — encoder goroutine likely leaked")
	}
}

// TestWrapDecodePassthroughForNone: codec=none must return the sink
// unchanged (no extra goroutines, no extra buffering) so callers pay
// nothing when compression is off.
func TestWrapDecodePassthroughForNone(t *testing.T) {
	t.Parallel()
	sink := &trackingSink{}
	out, err := xfer.WrapDecode(sink, wire.PreambleCompressionNone)
	if err != nil {
		t.Fatalf("WrapDecode: %v", err)
	}
	if out != sink {
		t.Fatalf("expected pass-through; got wrapper")
	}
}

// TestWrapDecodeZstdRoundTrip: writes from the wrapper land in the inner
// sink as decoded bytes. Locks in the public contract that backs the WASM
// in-memory receive path (an arbitrary io.WriteCloser, no path on disk).
func TestWrapDecodeZstdRoundTrip(t *testing.T) {
	t.Parallel()
	want := []byte("conduit zstd wrap-decode round trip")
	var encoded bytes.Buffer
	enc, err := zstd.NewWriter(&encoded)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(want); err != nil {
		t.Fatalf("encode write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("encode close: %v", err)
	}

	sink := &trackingSink{}
	out, err := xfer.WrapDecode(sink, wire.PreambleCompressionZstd)
	if err != nil {
		t.Fatalf("WrapDecode: %v", err)
	}
	if _, err := io.Copy(out, &encoded); err != nil {
		t.Fatalf("write to wrapper: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close wrapper: %v", err)
	}
	if got := sink.buf.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("decoded bytes:\n  got  %q\n  want %q", got, want)
	}
	if !sink.closed {
		t.Fatalf("wrapper Close did not propagate to inner sink")
	}
}

// TestWrapDecodeUnsupportedCodecLeavesSinkOpen: the contract is that on
// error the caller retains ownership of sink. Sinks like the WASM
// wasmTransferSink fire onEnd in Close; closing them on a setup failure
// would emit a phantom "transfer complete" before the error surfaces.
func TestWrapDecodeUnsupportedCodecLeavesSinkOpen(t *testing.T) {
	t.Parallel()
	sink := &trackingSink{}
	out, err := xfer.WrapDecode(sink, "lz77")
	if err == nil {
		t.Fatal("expected error for unsupported codec")
	}
	if out != nil {
		t.Fatalf("expected nil writer on error, got %T", out)
	}
	if sink.closed {
		t.Fatal("WrapDecode must not Close the sink on error")
	}
}

// TestPreambleRejectsUnknownCompression: the read side rejects values it
// does not recognize, which is what protects receivers from senders that
// announce a codec they cannot speak.
func TestPreambleRejectsUnknownCompression(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	pre := wire.Preamble{
		Kind:        wire.PreambleKindFile,
		Name:        "x",
		Size:        0,
		MIME:        "application/octet-stream",
		Compression: wire.PreambleCompressionNone,
	}
	if err := wire.WritePreamble(&buf, pre); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Mutate the JSON body to swap compression to an unknown codec.
	body := buf.Bytes()
	out := bytes.Replace(body, []byte(`"compression":"none"`), []byte(`"compression":"lz77"`), 1)
	if _, err := wire.ReadPreamble(bytes.NewReader(out)); err == nil {
		t.Fatal("expected error for unknown compression value")
	}
}
