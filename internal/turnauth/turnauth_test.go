package turnauth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
	"time"
)

func TestIssueDeterministic(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	secret := []byte("s3cret")
	iss, err := NewIssuer(secret, []string{"turn:example.com:3478"}, 10*time.Minute, "conduit", func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	got := iss.Issue()
	wantExpiry := now.Add(10 * time.Minute).Unix()
	wantUsername := "1700000600:conduit"
	if got.Username != wantUsername {
		t.Errorf("username = %q, want %q (expiry %d)", got.Username, wantUsername, wantExpiry)
	}
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(wantUsername))
	wantCred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if got.Credential != wantCred {
		t.Errorf("credential = %q, want %q", got.Credential, wantCred)
	}
	if got.TTL != 600 {
		t.Errorf("ttl = %d, want 600", got.TTL)
	}
	if len(got.URIs) != 1 || got.URIs[0] != "turn:example.com:3478" {
		t.Errorf("uris = %v, want [turn:example.com:3478]", got.URIs)
	}
}

func TestIssueNoPrefixInUsername(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	iss, err := NewIssuer([]byte("x"), nil, time.Minute, "", func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	got := iss.Issue()
	for _, r := range got.Username {
		if r == ':' {
			t.Errorf("username unexpectedly contains ':': %q", got.Username)
			break
		}
	}
}

func TestNewIssuerRequiresSecret(t *testing.T) {
	t.Parallel()
	if _, err := NewIssuer(nil, nil, time.Minute, "", time.Now); err == nil {
		t.Fatal("NewIssuer with nil secret = nil, want error")
	}
	if _, err := NewIssuer([]byte{}, nil, time.Minute, "", time.Now); err == nil {
		t.Fatal("NewIssuer with empty secret = nil, want error")
	}
}

func TestNewIssuerRequiresTTL(t *testing.T) {
	t.Parallel()
	if _, err := NewIssuer([]byte("x"), nil, 0, "", time.Now); err == nil {
		t.Fatal("NewIssuer with ttl=0 = nil, want error")
	}
	if _, err := NewIssuer([]byte("x"), nil, -time.Minute, "", time.Now); err == nil {
		t.Fatal("NewIssuer with negative ttl = nil, want error")
	}
}

func TestNewIssuerRequiresNow(t *testing.T) {
	t.Parallel()
	if _, err := NewIssuer([]byte("x"), nil, time.Minute, "", nil); err == nil {
		t.Fatal("NewIssuer with nil now = nil, want error")
	}
}
