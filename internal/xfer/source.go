// Package xfer builds conduit send sources and receive sinks from CLI-style
// inputs: --text, stdin, one or more paths, or a directory. A Source pairs a
// [wire.Preamble] describing the payload shape with an [io.ReadCloser] that
// yields the raw bytes to encrypt. A Sink decides how incoming bytes land on
// the receiver (single file, stdout, or tar-unpacked directory).
package xfer

import (
	"archive/tar"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/danielmmetz/conduit/internal/wire"
	gitignore "github.com/sabhiram/go-gitignore"
)

// Source is a streamable payload paired with its preamble.
type Source struct {
	Preamble wire.Preamble
	// Reader is an io.ReadCloser owned by the caller; it must be closed once
	// the transfer completes (successful or not). For pipe-backed sources
	// (tar streaming), Close is what joins the producer goroutine.
	Reader io.ReadCloser
}

// Close releases the underlying reader. Safe to call when Reader is nil.
func (s *Source) Close() error {
	if s == nil || s.Reader == nil {
		return nil
	}
	return s.Reader.Close()
}

// StdinMarker is the path string that means "read from stdin".
const StdinMarker = "-"

// SourceOptions configures how a Source is built. Compression chooses the
// codec applied to the user's bytes before they hit the wire. Progress, if
// non-nil, is invoked with the cumulative count of user-visible
// (uncompressed) bytes read from the source — counted upstream of the
// encoder so the caller sees the same byte numbers regardless of codec.
type SourceOptions struct {
	Compression CompressMode
	Progress    func(int64)
}

// OpenText builds a text Source from a literal string. The preamble carries
// the exact byte length so the receiver can default to stdout without
// knowing the content MIME. Auto compression on text always resolves to
// none (the encoder overhead exceeds the gain).
func OpenText(text string, opts SourceOptions) (*Source, error) {
	pre := wire.Preamble{
		Kind:        wire.PreambleKindText,
		Size:        int64(len(text)),
		MIME:        "text/plain; charset=utf-8",
		Compression: wire.PreambleCompressionNone,
	}
	rdr := io.NopCloser(strings.NewReader(text))
	return finalizeSource(&Source{Preamble: pre, Reader: rdr}, opts)
}

// OpenStdin builds a streaming Source from stdin. Size is unknown so the
// preamble records -1; the receiver cannot show a percentage bar for stdin
// sources, only a byte counter. Auto compression turns zstd on (we cannot
// peek at stdin to MIME-detect).
func OpenStdin(stdin io.Reader, opts SourceOptions) (*Source, error) {
	pre := wire.Preamble{
		Kind:        wire.PreambleKindFile,
		Name:        "stdin",
		Size:        -1,
		MIME:        "application/octet-stream",
		Compression: wire.PreambleCompressionNone,
	}
	rdr := io.NopCloser(stdin)
	return finalizeSource(&Source{Preamble: pre, Reader: rdr}, opts)
}

// OpenPaths resolves one or more positional paths into a Source. The rules:
//   - a single StdinMarker ("-") → OpenStdin.
//   - a single regular file → streaming single-file Source with exact Size.
//   - a single directory, or multiple paths (mixed files/dirs) → streaming
//     tar Source with Size == -1.
//
// When git is true, directory walks honor a .gitignore at each directory's
// root (sub-directory .gitignore files are not consulted in v1) and always
// skip the .git/ subtree. When git is false, the directory is sent verbatim.
//
// Paths are opened lazily where possible: for the single-file case we open
// and stat the file here so a missing path fails fast; for the tar case we
// validate each path exists, then build a piped tar-producer goroutine.
//
// opts.Compression chooses the codec; the chosen codec is recorded in
// Source.Preamble.Compression and (for zstd) the reader is wrapped so the
// caller's reads return the compressed stream. Preamble.Size keeps the
// user-visible (uncompressed) byte count regardless of codec, so the
// receiver's progress meter can render a percentage even when the wire
// length is hidden by compression. opts.Progress, if non-nil, is invoked
// with cumulative uncompressed byte counts — see [SourceOptions].
func OpenPaths(paths []string, stdin io.Reader, git bool, opts SourceOptions) (*Source, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("resolving send source: no paths given")
	}
	if len(paths) == 1 && paths[0] == StdinMarker {
		return OpenStdin(stdin, opts)
	}
	if slices.Contains(paths, StdinMarker) {
		return nil, fmt.Errorf("resolving send source: stdin %q cannot be combined with other paths", StdinMarker)
	}
	if len(paths) == 1 {
		info, err := os.Stat(paths[0])
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", paths[0], err)
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(paths[0])
			if err != nil {
				return nil, fmt.Errorf("opening %s: %w", paths[0], err)
			}
			pre := wire.Preamble{
				Kind:        wire.PreambleKindFile,
				Name:        filepath.Base(paths[0]),
				Size:        info.Size(),
				MIME:        guessMIME(paths[0]),
				Compression: wire.PreambleCompressionNone,
			}
			return finalizeSource(&Source{Preamble: pre, Reader: f}, opts)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("resolving send source: %s is neither a regular file nor a directory", paths[0])
		}
	}
	// Multi-path or single-directory → tar stream.
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("stat %s: %w", p, err)
		}
	}
	src, err := newTarSource(paths, git)
	if err != nil {
		return nil, fmt.Errorf("opening tar source: %w", err)
	}
	return finalizeSource(src, opts)
}

// finalizeSource applies the user's options to a freshly-built Source. It
// installs the progress counter (if any) on the inner reader so callbacks
// fire on uncompressed bytes, then picks the codec from the preamble shape
// and mode, wraps the reader if zstd, and records Compression on the
// preamble. Preamble.Size keeps the user-visible (uncompressed) byte count
// regardless of codec — it drives a continuous progress percentage on the
// receiver even when compression hides the wire byte count. If wrap fails,
// the Source's reader is closed and the error is returned.
func finalizeSource(src *Source, opts SourceOptions) (*Source, error) {
	if opts.Progress != nil {
		// Layer order matters: counter must sit between the user's reader
		// and the encoder so the count reflects uncompressed bytes.
		src.Reader = &countingReadCloser{r: src.Reader, cb: opts.Progress}
	}
	codec := pickCompression(src.Preamble, opts.Compression)
	src.Preamble.Compression = codec
	wrapped, err := wrapEncode(src.Reader, codec)
	if err != nil {
		return nil, fmt.Errorf("finalizing source: %w", err)
	}
	src.Reader = wrapped
	return src, nil
}

// countingReadCloser wraps an io.ReadCloser, invoking cb with the cumulative
// byte count after each successful Read. Used inside finalizeSource so the
// progress callback observes uncompressed (user-visible) byte counts.
type countingReadCloser struct {
	r  io.ReadCloser
	cb func(int64)
	n  int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.cb != nil {
			c.cb(c.n)
		}
	}
	return n, err
}

func (c *countingReadCloser) Close() error {
	if err := c.r.Close(); err != nil {
		return fmt.Errorf("closing counting reader: %w", err)
	}
	return nil
}

// newTarSource returns a Source whose Reader is the read end of an io.Pipe;
// a background goroutine walks the paths, writes PAX tar entries to the write
// end, and closes it on completion. Errors surface on the next Read.
//
// The producer goroutine terminates on any of: tar.Writer.Close error,
// successful finish, or the reader calling Close on the pipe read end
// (which causes subsequent writes to return io.ErrClosedPipe).
func newTarSource(paths []string, git bool) (*Source, error) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		tw := tar.NewWriter(pw)
		err := writeTarPaths(tw, paths, git)
		if cerr := tw.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			// CloseWithError propagates to the next pr.Read so rtc.Send
			// surfaces a useful failure rather than a silent truncated stream.
			_ = pw.CloseWithError(fmt.Errorf("streaming tar: %w", err))
		}
	}()

	name := filepath.Base(paths[0])
	if len(paths) > 1 {
		name = fmt.Sprintf("%s (+%d)", filepath.Base(paths[0]), len(paths)-1)
	}
	return &Source{
		Preamble: wire.Preamble{Kind: wire.PreambleKindTar, Name: name, Size: -1, MIME: "application/x-tar", Compression: wire.PreambleCompressionNone},
		Reader:   pr,
	}, nil
}

func writeTarPaths(tw *tar.Writer, paths []string, git bool) error {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("lstat %s: %w", p, err)
		}
		base := filepath.Base(p)
		if info.IsDir() {
			root := filepath.Clean(p)
			var ig *gitignore.GitIgnore
			if git {
				// CompileIgnoreFile returns a non-nil error when .gitignore is missing
				// or unreadable; treat both as "no patterns" and fall through to the
				// always-skip-.git rule below.
				ig, _ = gitignore.CompileIgnoreFile(filepath.Join(root, ".gitignore"))
			}
			err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
				if err != nil {
					return fmt.Errorf("walking %s: %w", path, err)
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return fmt.Errorf("rel %s: %w", path, err)
				}
				if git && rel != "." {
					if fi.IsDir() && fi.Name() == ".git" {
						return filepath.SkipDir
					}
					if ig != nil && ig.MatchesPath(filepath.ToSlash(rel)) {
						if fi.IsDir() {
							return filepath.SkipDir
						}
						return nil
					}
				}
				name := base
				if rel != "." {
					name = filepath.Join(base, rel)
				}
				return writeTarEntry(tw, path, filepath.ToSlash(name), fi)
			})
			if err != nil {
				return err
			}
			continue
		}
		if err := writeTarEntry(tw, p, base, info); err != nil {
			return err
		}
	}
	return nil
}

func writeTarEntry(tw *tar.Writer, path, name string, info os.FileInfo) error {
	// Symlinks and other special files are skipped on the sender to keep the
	// receive-side traversal guard simple (see Unpack). Regular files and
	// directories cover the v1 use cases.
	if !info.Mode().IsRegular() && !info.IsDir() {
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("tar header %s: %w", path, err)
	}
	hdr.Name = name
	if info.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
		hdr.Name += "/"
	}
	hdr.Format = tar.FormatPAX
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing tar header %s: %w", path, err)
	}
	if info.IsDir() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.Copy(tw, f); err != nil {
		return fmt.Errorf("copying %s: %w", path, err)
	}
	return nil
}

func guessMIME(path string) string {
	if m := mime.TypeByExtension(filepath.Ext(path)); m != "" {
		return m
	}
	return "application/octet-stream"
}
