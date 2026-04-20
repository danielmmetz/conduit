package xfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/danielmmetz/conduit/internal/wire"
)

// StdoutMarker is the -o value (and positional marker) that means "write to
// stdout" rather than a path.
const StdoutMarker = "-"

// SinkOptions describe how the CLI wants to consume an incoming payload.
// OutPath "" → default routing (filename from preamble for file; extract to
// CWD for tar; stdout for text). OutPath "-" → always stdout. Other → path
// override. Stdout is the writer for stdout-fallback cases so tests can plug
// in a buffer instead of os.Stdout.
type SinkOptions struct {
	OutPath string
	Stdout  io.Writer
}

// OpenSink returns an io.WriteCloser that consumes the incoming payload
// according to preamble and opts. For tar payloads the returned writer is an
// adapter that extracts tar entries under the chosen directory root, with
// traversal guards (no absolute paths, no ".." escapes, no symlinks).
//
// Close must be called on the returned writer so tar extraction state (the
// tar.Reader position) is verified and, for single-file sinks, the file is
// closed before returning.
func OpenSink(pre wire.Preamble, opts SinkOptions) (io.WriteCloser, error) {
	switch pre.Kind {
	case wire.PreambleKindFile, wire.PreambleKindText:
		return openFileSink(pre, opts)
	case wire.PreambleKindTar:
		return openTarSink(opts)
	default:
		return nil, fmt.Errorf("opening sink: unknown preamble kind %q", pre.Kind)
	}
}

func openFileSink(pre wire.Preamble, opts SinkOptions) (io.WriteCloser, error) {
	switch opts.OutPath {
	case StdoutMarker:
		return stdoutWriter(opts), nil
	case "":
		if pre.Kind == wire.PreambleKindText || pre.Name == "" || pre.Name == "stdin" {
			return stdoutWriter(opts), nil
		}
		name := filepath.Base(pre.Name)
		if name == "." || name == "/" || name == "" {
			return nil, fmt.Errorf("opening sink: peer sent unusable filename %q", pre.Name)
		}
		return createFile(name)
	default:
		return createFile(opts.OutPath)
	}
}

func stdoutWriter(opts SinkOptions) io.WriteCloser {
	w := opts.Stdout
	if w == nil {
		w = os.Stdout
	}
	return nopWriteCloser{w: w}
}

func createFile(path string) (io.WriteCloser, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	return f, nil
}

// nopWriteCloser is like io.NopCloser but for writers (stdlib provides the
// reader variant only).
type nopWriteCloser struct{ w io.Writer }

func (n nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n nopWriteCloser) Close() error                { return nil }

// openTarSink resolves the extraction root and returns a writer that consumes
// the incoming tar stream. OutPath == "-" is rejected: streaming a tar to
// stdout is plausible but today's CLI unpacks, and the receiver has no good
// default for a tar-to-stdout shape.
func openTarSink(opts SinkOptions) (io.WriteCloser, error) {
	if opts.OutPath == StdoutMarker {
		return nil, fmt.Errorf("opening sink: cannot extract a directory to stdout")
	}
	root := opts.OutPath
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("opening sink: %w", err)
		}
		root = cwd
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("opening sink: mkdir %s: %w", root, err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("opening sink: abs %s: %w", root, err)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := unpackTar(pr, abs)
		// Closing pr with the error unblocks any pending pw.Write in the
		// caller so io.Copy can surface the extractor's error rather than
		// deadlocking until Close is called.
		if err != nil {
			_ = pr.CloseWithError(err)
		} else {
			_ = pr.Close()
		}
		done <- err
	}()
	return &tarSink{pw: pw, done: done}, nil
}

// tarSink accepts arbitrary bytes via Write and forwards them into the pipe
// feeding a background tar extractor. Close shuts the pipe and joins the
// extractor so tar errors (traversal attempts, short reads) are surfaced to
// the caller.
type tarSink struct {
	pw   *io.PipeWriter
	done chan error
}

func (t *tarSink) Write(p []byte) (int, error) {
	n, err := t.pw.Write(p)
	if err != nil {
		return n, fmt.Errorf("feeding tar extractor: %w", err)
	}
	return n, nil
}

func (t *tarSink) Close() error {
	_ = t.pw.Close()
	if err := <-t.done; err != nil {
		return fmt.Errorf("closing tar sink: %w", err)
	}
	return nil
}

// unpackTar reads a tar stream from r and writes each entry under root.
// Entries outside root (absolute paths, "../" escapes) are rejected; symlinks
// are refused because a malicious tar can point a symlink outside root and
// then write through it on a follow-up entry.
func unpackTar(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		target, err := safeJoin(root, hdr.Name)
		if err != nil {
			return fmt.Errorf("tar entry %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fileMode(hdr.Mode, 0o755)); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(hdr.Mode, 0o644))
			if err != nil {
				return fmt.Errorf("creating %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("writing %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("closing %s: %w", target, err)
			}
		default:
			// Skip symlinks, devices, fifos, sockets. A noisy skip is
			// preferable to silently trusting data from an untrusted peer.
			return fmt.Errorf("tar entry %q: unsupported type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

// safeJoin returns root/name guarded against traversal: name must be a
// relative path with no absolute prefix and no ".." component that escapes
// root after cleaning. Filesystem-level Abs handles symlink-in-root tricks
// because we never resolve symlinks when writing (O_CREATE|O_TRUNC opens the
// literal path; if a symlink exists at target we refuse rather than follow).
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path not allowed: %q", name)
	}
	// Clean the name as-is (no forced "/" prefix): if the entry starts with
	// or resolves to a "../" component, the cleaned form begins with ".."
	// and we reject explicitly rather than silently pinning inside root.
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes root: %q", name)
	}
	target := filepath.Join(root, clean)
	// Extra belt-and-braces: if target doesn't start with root after cleaning
	// (symlink shenanigans), reject.
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", target, err)
	}
	rootWithSep := root
	if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
		rootWithSep += string(filepath.Separator)
	}
	if absTarget != root && !strings.HasPrefix(absTarget, rootWithSep) {
		return "", fmt.Errorf("path escapes root: %q", name)
	}
	// Refuse to overwrite an existing symlink: O_TRUNC on a symlink follows
	// it to the target, which could be outside root.
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("refusing to overwrite symlink %q", name)
	}
	return target, nil
}

func fileMode(m int64, def os.FileMode) os.FileMode {
	if m == 0 {
		return def
	}
	return os.FileMode(m) & 0o777
}
