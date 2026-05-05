package wire

import (
	"slices"
	"strings"
	"testing"
)

func TestGenerateAndParseCode(t *testing.T) {
	t.Parallel()
	code, err := GenerateCode(42)
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	if code.Slot != 42 {
		t.Errorf("code.Slot = %d, want 42", code.Slot)
	}
	if len(code.Words) != DefaultCodeWords {
		t.Errorf("code.Words = %v, want %d words", code.Words, DefaultCodeWords)
	}
	rendered := code.String()
	parts := strings.Split(rendered, "-")
	if len(parts) != 1+DefaultCodeWords {
		t.Fatalf("code.String = %q, want slot + %d words", rendered, DefaultCodeWords)
	}
	parsed, err := ParseCode(rendered)
	if err != nil {
		t.Fatalf("ParseCode: %v", err)
	}
	if parsed.Slot != code.Slot {
		t.Errorf("parsed.Slot = %d, want %d", parsed.Slot, code.Slot)
	}
	if !slices.Equal(parsed.Words, code.Words) {
		t.Errorf("parsed.Words = %v, want %v", parsed.Words, code.Words)
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
