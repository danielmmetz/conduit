package wire

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestPreambleRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		pre  Preamble
	}{
		{"file", Preamble{Kind: PreambleKindFile, Name: "report.pdf", Size: 12345, MIME: "application/pdf", Compression: PreambleCompressionNone}},
		{"text", Preamble{Kind: PreambleKindText, Size: 5, Compression: PreambleCompressionNone}},
		{"tar streaming", Preamble{Kind: PreambleKindTar, Name: "src", Size: -1, Compression: PreambleCompressionNone}},
		{"file zstd", Preamble{Kind: PreambleKindFile, Name: "logs.txt", Size: 4096, MIME: "text/plain", Compression: PreambleCompressionZstd}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WritePreamble(&buf, c.pre); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := ReadPreamble(&buf)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got != c.pre {
				t.Fatalf("round trip: got %+v, want %+v", got, c.pre)
			}
		})
	}
}

func TestReadPreambleTrailingBytes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WritePreamble(&buf, Preamble{Kind: PreambleKindFile, Name: "a", Size: 3, Compression: PreambleCompressionNone}); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf.WriteString("payload") // simulates streaming payload after preamble
	_, err := ReadPreamble(&buf)
	if err != nil {
		t.Fatalf("read preamble: %v", err)
	}
	remaining, err := io.ReadAll(&buf)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if string(remaining) != "payload" {
		t.Fatalf("trailing bytes = %q, want %q", remaining, "payload")
	}
}

func TestReadPreambleRejectsOversized(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxPreambleBody+1)
	buf.Write(hdr[:])
	_, err := ReadPreamble(&buf)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("ReadPreamble oversized = %v, want max-size error", err)
	}
}

func TestReadPreambleShortBody(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 16)
	buf.Write(hdr[:])
	buf.WriteString("short") // only 5 bytes of body, announced 16
	_, err := ReadPreamble(&buf)
	if err == nil {
		t.Fatalf("ReadPreamble short body = nil, want error")
	}
}

func TestWritePreambleRejectsOversized(t *testing.T) {
	t.Parallel()
	big := Preamble{Kind: PreambleKindFile, Name: strings.Repeat("x", MaxPreambleBody+1), Compression: PreambleCompressionNone}
	err := WritePreamble(io.Discard, big)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("WritePreamble oversized = %v, want max-size error", err)
	}
}

func TestValidatePreambleAcceptsKnownCompression(t *testing.T) {
	t.Parallel()
	for _, c := range []string{PreambleCompressionNone, PreambleCompressionZstd} {
		if err := ValidatePreamble(Preamble{Kind: PreambleKindFile, Compression: c}); err != nil {
			t.Errorf("ValidatePreamble compression=%q = %v, want nil", c, err)
		}
	}
}

func TestValidatePreambleRejectsUnknownCompression(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"", "lz77", "gzip"} {
		if err := ValidatePreamble(Preamble{Kind: PreambleKindFile, Compression: c}); err == nil {
			t.Errorf("ValidatePreamble compression=%q = nil, want error", c)
		}
	}
}
