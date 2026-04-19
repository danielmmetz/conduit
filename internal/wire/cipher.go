package wire

import (
	"fmt"
	"io"

	"filippo.io/age"
)

// ageWorkFactor is the scrypt logN used when writing payloads. K already has
// SessionKeySize*8 bits of entropy, so scrypt's stretching is not needed for
// security; use the minimum work factor so each encrypted stream (signaling
// frames and payload) does not pile up CPU on hosts running many sessions or
// tests in parallel.
const ageWorkFactor = 1

// Encrypt returns an io.WriteCloser that wraps dst with age's scrypt recipient
// keyed by the 32-byte session key K. Caller must Close to finalize.
func Encrypt(dst io.Writer, key []byte) (io.WriteCloser, error) {
	if len(key) != SessionKeySize {
		return nil, fmt.Errorf("encrypt: key is %d bytes, want %d", len(key), SessionKeySize)
	}
	r, err := age.NewScryptRecipient(string(key))
	if err != nil {
		return nil, fmt.Errorf("building scrypt recipient: %w", err)
	}
	r.SetWorkFactor(ageWorkFactor)
	wc, err := age.Encrypt(dst, r)
	if err != nil {
		return nil, fmt.Errorf("starting age encrypt: %w", err)
	}
	return wc, nil
}

// Decrypt returns a reader yielding plaintext read from src, using the 32-byte
// session key K as age's scrypt passphrase. The returned reader streams; it
// does not need to be closed.
func Decrypt(src io.Reader, key []byte) (io.Reader, error) {
	if len(key) != SessionKeySize {
		return nil, fmt.Errorf("decrypt: key is %d bytes, want %d", len(key), SessionKeySize)
	}
	id, err := age.NewScryptIdentity(string(key))
	if err != nil {
		return nil, fmt.Errorf("building scrypt identity: %w", err)
	}
	pr, err := age.Decrypt(src, id)
	if err != nil {
		return nil, fmt.Errorf("starting age decrypt: %w", err)
	}
	return pr, nil
}
