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
		{"file", Preamble{Kind: PreambleKindFile, Name: "report.pdf", Size: 12345, MIME: "application/pdf"}},
		{"text", Preamble{Kind: PreambleKindText, Size: 5}},
		{"tar streaming", Preamble{Kind: PreambleKindTar, Name: "src", Size: -1}},
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
	if err := WritePreamble(&buf, Preamble{Kind: PreambleKindFile, Name: "a", Size: 3}); err != nil {
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
	big := Preamble{Kind: PreambleKindFile, Name: strings.Repeat("x", MaxPreambleBody+1)}
	err := WritePreamble(io.Discard, big)
	if err == nil || !strings.Contains(err.Error(), "max") {
		t.Fatalf("WritePreamble oversized = %v, want max-size error", err)
	}
}
