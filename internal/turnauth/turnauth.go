// Package turnauth issues RFC 8489 short-term TURN credentials.
//
// username is "<unix_expiry>:<prefix>"; credential is the base64 HMAC-SHA1 of
// the username keyed by Secret. TURN servers configured with the same secret
// accept the credential until the embedded expiry has passed.
package turnauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"
)

// Issuer mints short-lived TURN credentials. Construct with NewIssuer.
type Issuer struct {
	secret []byte
	uris   []string
	ttl    time.Duration
	prefix string
	now    func() time.Time
}

// NewIssuer builds an Issuer. secret must be non-empty, ttl positive, and now
// non-nil (pass time.Now from production code). prefix may be empty (username
// is expiry only). uris may be empty.
func NewIssuer(secret []byte, uris []string, ttl time.Duration, prefix string, now func() time.Time) (*Issuer, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("secret must be non-empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("ttl must be positive, received %v", ttl)
	}
	if now == nil {
		return nil, fmt.Errorf("now must be non-nil")
	}
	secCopy := make([]byte, len(secret))
	copy(secCopy, secret)
	urisCopy := make([]string, len(uris))
	copy(urisCopy, uris)
	return &Issuer{
		secret: secCopy,
		uris:   urisCopy,
		ttl:    ttl,
		prefix: prefix,
		now:    now,
	}, nil
}

// Creds are the fields a client needs to hand to a TURN server or WebRTC stack.
type Creds struct {
	URIs       []string `json:"uris"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	TTL        int      `json:"ttl"`
}

// Issue returns a fresh credential.
func (iss *Issuer) Issue() Creds {
	expiry := iss.now().Add(iss.ttl).Unix()
	username := strconv.FormatInt(expiry, 10)
	if iss.prefix != "" {
		username = username + ":" + iss.prefix
	}
	mac := hmac.New(sha1.New, iss.secret)
	mac.Write([]byte(username))
	return Creds{
		URIs:       iss.uris,
		Username:   username,
		Credential: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		TTL:        int(iss.ttl / time.Second),
	}
}
