package xfer_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/danielmmetz/conduit/internal/xfer"
)

func TestOpenTextSource(t *testing.T) {
	t.Parallel()
	s := xfer.OpenText("hello world")
	defer s.Close()
	if s.Preamble.Kind != wire.PreambleKindText || s.Preamble.Size != 11 {
		t.Fatalf("preamble = %+v", s.Preamble)
	}
	b, err := io.ReadAll(s.Reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "hello world" {
		t.Fatalf("payload = %q", b)
	}
}

func TestOpenPathsSingleFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "data.txt")
	payload := []byte("conduit phase 7\n")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := xfer.OpenPaths([]string{path}, nil, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Kind != wire.PreambleKindFile || s.Preamble.Name != "data.txt" || s.Preamble.Size != int64(len(payload)) {
		t.Fatalf("preamble = %+v", s.Preamble)
	}
	got, err := io.ReadAll(s.Reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}
}

func TestOpenPathsStdin(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("from stdin")
	s, err := xfer.OpenPaths([]string{xfer.StdinMarker}, in, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Kind != wire.PreambleKindFile || s.Preamble.Size != -1 {
		t.Fatalf("preamble = %+v", s.Preamble)
	}
	got, err := io.ReadAll(s.Reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "from stdin" {
		t.Fatalf("payload = %q", got)
	}
}

func TestOpenPathsDirectoryStreamsTar(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "alpha")
	mustWrite(t, filepath.Join(root, "nested", "b.txt"), "bravo")

	s, err := xfer.OpenPaths([]string{root}, nil, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Kind != wire.PreambleKindTar || s.Preamble.Size != -1 {
		t.Fatalf("preamble = %+v", s.Preamble)
	}

	// Extract the tar via OpenSink and verify the files round-trip.
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
	checkFile(t, filepath.Join(outRoot, base, "a.txt"), "alpha")
	checkFile(t, filepath.Join(outRoot, base, "nested", "b.txt"), "bravo")
}

func TestOpenPathsMultipleFilesStreamsTar(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	p1 := filepath.Join(tmp, "one.txt")
	p2 := filepath.Join(tmp, "two.txt")
	mustWrite(t, p1, "1")
	mustWrite(t, p2, "22")

	s, err := xfer.OpenPaths([]string{p1, p2}, nil, false)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.Preamble.Kind != wire.PreambleKindTar {
		t.Fatalf("kind = %q", s.Preamble.Kind)
	}
	out := t.TempDir()
	sink, err := xfer.OpenSink(s.Preamble, xfer.SinkOptions{OutPath: out})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, s.Reader); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}
	checkFile(t, filepath.Join(out, "one.txt"), "1")
	checkFile(t, filepath.Join(out, "two.txt"), "22")
}

func TestOpenPathsStdinWithOtherPaths(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	p := filepath.Join(tmp, "a.txt")
	mustWrite(t, p, "x")
	_, err := xfer.OpenPaths([]string{xfer.StdinMarker, p}, nil, false)
	if err == nil {
		t.Fatal("expected error when mixing stdin with other paths")
	}
}

func TestOpenPathsDirectoryGitIgnoreSkipsPatterns(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".gitignore"), "node_modules\n*.log\n")
	mustWrite(t, filepath.Join(root, "keep.txt"), "kept")
	mustWrite(t, filepath.Join(root, "trace.log"), "should be skipped")
	mustWrite(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "skipped subtree")

	out := extractDirSend(t, root, true)
	base := filepath.Base(root)
	checkFile(t, filepath.Join(out, base, "keep.txt"), "kept")
	checkAbsent(t, filepath.Join(out, base, "trace.log"))
	checkAbsent(t, filepath.Join(out, base, "node_modules"))
}

func TestOpenPathsDirectoryGitIgnoreSkipsGitDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "README"), "hello")
	mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(root, ".git", "objects", "pack", "x.pack"), "binary")

	out := extractDirSend(t, root, true)
	base := filepath.Base(root)
	checkFile(t, filepath.Join(out, base, "README"), "hello")
	checkAbsent(t, filepath.Join(out, base, ".git"))
}

func TestOpenPathsDirectoryGitFalseSendsEverything(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, ".gitignore"), "skipped.txt\n")
	mustWrite(t, filepath.Join(root, "skipped.txt"), "with --git=true this would be skipped")
	mustWrite(t, filepath.Join(root, ".git", "HEAD"), "ref")

	out := extractDirSend(t, root, false)
	base := filepath.Base(root)
	checkFile(t, filepath.Join(out, base, "skipped.txt"), "with --git=true this would be skipped")
	checkFile(t, filepath.Join(out, base, ".git", "HEAD"), "ref")
}

func TestOpenSinkRejectsTarTraversal(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "../evil.txt", Mode: 0o644, Size: 5, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := tw.Write([]byte("nope!")); err != nil {
		t.Fatalf("body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}

	root := t.TempDir()
	sink, err := xfer.OpenSink(wire.Preamble{Kind: wire.PreambleKindTar}, xfer.SinkOptions{OutPath: root})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, &buf); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		// Extractor may detect traversal mid-stream and close the pipe; either
		// an immediate copy error or a Close error is acceptable.
		t.Logf("copy surfaced: %v", err)
	}
	closeErr := sink.Close()
	if closeErr == nil {
		t.Fatal("expected tar traversal to be rejected, got nil")
	}
	if !strings.Contains(closeErr.Error(), "escape") && !strings.Contains(closeErr.Error(), "absolute") {
		t.Fatalf("unexpected error %q (want traversal error)", closeErr)
	}
	// And the evil file must not have been written under root or its parent.
	parent := filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(parent, "evil.txt")); err == nil {
		t.Fatalf("evil.txt was written outside root")
	}
}

func TestOpenSinkRejectsAbsolutePath(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "/etc/passwd", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	root := t.TempDir()
	sink, err := xfer.OpenSink(wire.Preamble{Kind: wire.PreambleKindTar}, xfer.SinkOptions{OutPath: root})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	_, _ = io.Copy(sink, &buf)
	if err := sink.Close(); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("Close = %v, want absolute-path rejection", err)
	}
}

func TestOpenSinkRejectsSymlinkEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("header: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	root := t.TempDir()
	sink, err := xfer.OpenSink(wire.Preamble{Kind: wire.PreambleKindTar}, xfer.SinkOptions{OutPath: root})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	_, _ = io.Copy(sink, &buf)
	if err := sink.Close(); err == nil {
		t.Fatal("expected symlink rejection, got nil")
	}
}

func TestOpenSinkFileDefaultsToPreambleName(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	pre := wire.Preamble{Kind: wire.PreambleKindFile, Name: "hello.bin", Size: 5}
	sink, err := xfer.OpenSink(pre, xfer.SinkOptions{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sink.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	checkFile(t, filepath.Join(tmp, "hello.bin"), "hello")
}

func TestOpenSinkTextStdoutPadsMissingNewline(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	pre := wire.Preamble{Kind: wire.PreambleKindText, Size: 5}
	sink, err := xfer.OpenSink(pre, xfer.SinkOptions{OutPath: xfer.StdoutMarker, Stdout: &out})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sink.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello\n")
	}
}

func TestOpenSinkTextStdoutKeepsExistingNewline(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	pre := wire.Preamble{Kind: wire.PreambleKindText, Size: 6}
	sink, err := xfer.OpenSink(pre, xfer.SinkOptions{OutPath: xfer.StdoutMarker, Stdout: &out})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sink.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello\n")
	}
}

func TestOpenSinkFileStdoutNoPadding(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	pre := wire.Preamble{Kind: wire.PreambleKindFile, Name: "blob.bin", Size: 5}
	sink, err := xfer.OpenSink(pre, xfer.SinkOptions{OutPath: xfer.StdoutMarker, Stdout: &out})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := sink.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if out.String() != "hello" {
		t.Fatalf("stdout = %q, want %q", out.String(), "hello")
	}
}

func TestOpenSinkTarStdoutRejected(t *testing.T) {
	t.Parallel()
	_, err := xfer.OpenSink(wire.Preamble{Kind: wire.PreambleKindTar}, xfer.SinkOptions{OutPath: xfer.StdoutMarker})
	if err == nil {
		t.Fatal("expected error for tar→stdout")
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func checkAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists, want absent (stat err=%v)", path, err)
	}
}

// extractDirSend runs OpenPaths on root, pipes the tar stream through OpenSink,
// and returns the extraction directory. Used by gitignore tests to assert which
// files survive the walk.
func extractDirSend(t *testing.T, root string, git bool) string {
	t.Helper()
	s, err := xfer.OpenPaths([]string{root}, nil, git)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	out := t.TempDir()
	sink, err := xfer.OpenSink(s.Preamble, xfer.SinkOptions{OutPath: out})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	if _, err := io.Copy(sink, s.Reader); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("close sink: %v", err)
	}
	return out
}
