package client

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danielmmetz/conduit/internal/wire"
)

// TestDecodePreambleAcceptsKnownCompression: sanity check that the
// streaming receive path does not over-reject valid codec values.
func TestDecodePreambleAcceptsKnownCompression(t *testing.T) {
	t.Parallel()
	for _, c := range []string{wire.PreambleCompressionNone, wire.PreambleCompressionZstd} {
		body, err := json.Marshal(wire.Preamble{Kind: wire.PreambleKindFile, Compression: c})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := decodePreamble(body); err != nil {
			t.Errorf("decodePreamble compression=%q = %v, want nil", c, err)
		}
	}
}

// TestDecodePreambleRejectsUnknownCompression: the streaming receive path
// must reject unknown codec values at the same boundary as
// wire.ReadPreamble. Without this, an unsupported codec would slip
// through to xfer.WrapDecode, where the failure would be less precise
// and (for sinks with side-effecting Close) harder to recover from.
func TestDecodePreambleRejectsUnknownCompression(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"", "lz77", "gzip"} {
		body, err := json.Marshal(wire.Preamble{Kind: wire.PreambleKindFile, Compression: c})
		if err != nil {
			t.Fatalf("marshal compression=%q: %v", c, err)
		}
		_, err = decodePreamble(body)
		if err == nil {
			t.Errorf("decodePreamble compression=%q = nil, want error", c)
			continue
		}
		if !strings.Contains(err.Error(), "compression") {
			t.Errorf("decodePreamble compression=%q error = %v, want mention of compression", c, err)
		}
	}
}
