package wire

import (
	"slices"
	"strings"
	"testing"
)

func TestFormatAndParseCode(t *testing.T) {
	t.Parallel()
	code, err := FormatCode(42)
	if err != nil {
		t.Fatalf("FormatCode: %v", err)
	}
	parts := strings.Split(code, "-")
	if len(parts) != 1+DefaultCodeWords {
		t.Fatalf("FormatCode = %q, want slot + %d words", code, DefaultCodeWords)
	}

	parsed, err := ParseCode(code)
	if err != nil {
		t.Fatalf("ParseCode: %v", err)
	}
	if parsed.Slot != 42 {
		t.Errorf("parsed.Slot = %d, want 42", parsed.Slot)
	}
	if len(parsed.Words) != DefaultCodeWords {
		t.Errorf("parsed.Words = %v, want %d words", parsed.Words, DefaultCodeWords)
	}
	if got := parsed.String(); got != code {
		t.Errorf("roundtrip: got %q, want %q", got, code)
	}
}

func TestParseCodeValid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantSlot  uint32
		wantWords []string
	}{
		{"42", 42, nil},
		{"42-ice-cream-monkey", 42, []string{"ice", "cream", "monkey"}},
		{"1", 1, nil},
		{"1-WORD", 1, []string{"word"}},
	}
	for _, c := range cases {
		got, err := ParseCode(c.in)
		if err != nil {
			t.Errorf("ParseCode(%q) err = %v", c.in, err)
			continue
		}
		if got.Slot != c.wantSlot {
			t.Errorf("ParseCode(%q).Slot = %d, want %d", c.in, got.Slot, c.wantSlot)
		}
		if !slices.Equal(got.Words, c.wantWords) {
			t.Errorf("ParseCode(%q).Words = %v, want %v", c.in, got.Words, c.wantWords)
		}
	}
}

func TestParseCodeInvalid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"",
		"abc",
		"-1",
		"1--word",
	}
	for _, in := range cases {
		if got, err := ParseCode(in); err == nil {
			t.Errorf("ParseCode(%q) = %+v, want error", in, got)
		}
	}
}
