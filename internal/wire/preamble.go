package wire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// PreambleKind discriminates the shape of the payload that follows the preamble.
const (
	PreambleKindFile = "file" // single file; Size is exact
	PreambleKindTar  = "tar"  // PAX tar stream of files and/or a directory tree
	PreambleKindText = "text" // literal utf-8 text; Size is exact
)

// Compression discriminates how the post-preamble bytes are encoded. "none"
// passes the user payload through verbatim; "zstd" wraps it in a zstd stream
// that the receiver decodes before reaching its sink. The field is required:
// senders must set it explicitly and receivers reject unknown values.
const (
	PreambleCompressionNone = "none"
	PreambleCompressionZstd = "zstd"
)

// Preamble is the first frame inside the age-encrypted payload stream. It
// describes what the remaining bytes are so the receiver can pick a sink
// (single file, tar extractor, stdout) without the server or relay ever
// seeing filenames or sizes.
//
// Size is the user-visible (uncompressed) byte count and is -1 when not known
// up-front (streaming stdin or a streaming tar). Compression does not change
// Size: the receiver runs wire bytes through the announced decoder and tracks
// progress in decoded bytes against this number, so the meter advances in the
// same units the user supplied. Receivers must treat -1 as "unknown" rather
// than trusting it as a length.
type Preamble struct {
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Size        int64  `json:"size"`
	MIME        string `json:"mime,omitempty"`
	Compression string `json:"compression"`
}

// MaxPreambleBody caps the encoded JSON size. Preambles carry at most a
// filename and MIME type; a few KiB is ample and bounds worst-case decode
// cost when peers send nonsense. Exported so the client layer (which reads
// the preamble via a streaming state machine rather than [ReadPreamble])
// can apply the same ceiling without duplicating the constant.
const MaxPreambleBody = 4 * 1024

// ValidatePreamble checks that p is well-formed for transmission. Centralised
// so [WritePreamble], [ReadPreamble], and the streaming receive decoder in
// internal/client all enforce the same rules — without this, the streaming
// path would silently accept fields the bulk decoder would reject.
func ValidatePreamble(p Preamble) error {
	switch p.Compression {
	case PreambleCompressionNone, PreambleCompressionZstd:
		return nil
	default:
		return fmt.Errorf("invalid compression %q", p.Compression)
	}
}

// WritePreamble writes a length-prefixed JSON preamble to w. The frame layout
// is [uint32 big-endian body length][body bytes]. Callers are expected to pass
// an age-encrypt writer here so the preamble inherits payload confidentiality.
func WritePreamble(w io.Writer, p Preamble) error {
	if err := ValidatePreamble(p); err != nil {
		return fmt.Errorf("encoding preamble: %w", err)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshaling preamble: %w", err)
	}
	if len(body) > MaxPreambleBody {
		return fmt.Errorf("encoding preamble: body is %d bytes, max %d", len(body), MaxPreambleBody)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("writing preamble header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("writing preamble body: %w", err)
	}
	return nil
}

// ReadPreamble reads the length-prefixed JSON preamble from r. It returns an
// error if the frame header or body is short, the announced length exceeds
// MaxPreambleBody, or the body is not valid JSON for Preamble.
func ReadPreamble(r io.Reader) (Preamble, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Preamble{}, fmt.Errorf("reading preamble header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxPreambleBody {
		return Preamble{}, fmt.Errorf("reading preamble body: announced %d bytes, max %d", n, MaxPreambleBody)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Preamble{}, fmt.Errorf("reading preamble body: %w", err)
	}
	var p Preamble
	if err := json.Unmarshal(body, &p); err != nil {
		return Preamble{}, fmt.Errorf("decoding preamble: %w", err)
	}
	if err := ValidatePreamble(p); err != nil {
		return Preamble{}, fmt.Errorf("decoding preamble: %w", err)
	}
	return p, nil
}
