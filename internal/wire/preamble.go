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

// Preamble is the first frame inside the age-encrypted payload stream. It
// describes what the remaining bytes are so the receiver can pick a sink
// (single file, tar extractor, stdout) without the server or relay ever
// seeing filenames or sizes.
//
// Size is -1 when the payload length is not known up-front — streaming stdin
// or a streaming tar. Receivers must treat -1 as "unknown" rather than
// trusting it as a length.
type Preamble struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size"`
	MIME string `json:"mime,omitempty"`
}

// MaxPreambleBody caps the encoded JSON size. Preambles carry at most a
// filename and MIME type; a few KiB is ample and bounds worst-case decode
// cost when peers send nonsense. Exported so the client layer (which reads
// the preamble via a streaming state machine rather than [ReadPreamble])
// can apply the same ceiling without duplicating the constant.
const MaxPreambleBody = 4 * 1024

// WritePreamble writes a length-prefixed JSON preamble to w. The frame layout
// is [uint32 big-endian body length][body bytes]. Callers are expected to pass
// an age-encrypt writer here so the preamble inherits payload confidentiality.
func WritePreamble(w io.Writer, p Preamble) error {
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
	return p, nil
}
