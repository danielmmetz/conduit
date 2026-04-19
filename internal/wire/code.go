package wire

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// DefaultCodeWords is the number of words appended to the slot id in the code.
const DefaultCodeWords = 3

// FormatCode composes the user-visible code "<slot>-word-word-word".
// It reads DefaultCodeWords * 2 bytes of entropy from crypto/rand.
func FormatCode(slot uint32) (string, error) {
	words, err := randomWords(DefaultCodeWords)
	if err != nil {
		return "", fmt.Errorf("generating words: %w", err)
	}
	return fmt.Sprintf("%d-%s", slot, strings.Join(words, "-")), nil
}

// Code is a parsed short code: the numeric slot id and the word portion.
type Code struct {
	Slot  uint32
	Words []string
}

// Password returns the portion of the code used as the PAKE password
// ("word-word-word"). The slot id is not included; it is communicated
// separately via rendezvous and used as the SPAKE2 appID.
func (c Code) Password() string {
	return strings.Join(c.Words, "-")
}

// String renders the full "<slot>-word-word-word" form.
func (c Code) String() string {
	if len(c.Words) == 0 {
		return strconv.FormatUint(uint64(c.Slot), 10)
	}
	return fmt.Sprintf("%d-%s", c.Slot, strings.Join(c.Words, "-"))
}

// ParseCode accepts "<slot>" or "<slot>-word-word-word..." and returns the
// decomposed form. Words are lower-cased for comparison; the wordlist itself
// is not validated (a typo surfaces as a PAKE failure, not a parse error).
func ParseCode(s string) (Code, error) {
	if s == "" {
		return Code{}, fmt.Errorf("empty code")
	}
	parts := strings.Split(s, "-")
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return Code{}, fmt.Errorf("invalid slot %q: %w", parts[0], err)
	}
	c := Code{Slot: uint32(n)}
	for _, w := range parts[1:] {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			return Code{}, fmt.Errorf("empty word in code %q", s)
		}
		c.Words = append(c.Words, w)
	}
	return c, nil
}

func randomWords(n int) ([]string, error) {
	buf := make([]byte, 2*n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("reading random bytes: %w", err)
	}
	out := make([]string, n)
	for i := range n {
		idx := binary.BigEndian.Uint16(buf[2*i:2*i+2]) % uint16(len(EFFShortWordlist))
		out[i] = EFFShortWordlist[idx]
	}
	return out, nil
}
