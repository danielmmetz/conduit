package turnauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cfFixtureResponse is the body shape Cloudflare's
// generate-ice-servers endpoint returns. Pulled from the public docs.
const cfFixtureResponse = `{
  "iceServers": [
    {"urls": ["stun:stun.cloudflare.com:3478", "stun:stun.cloudflare.com:53"]},
    {
      "urls": [
        "turn:turn.cloudflare.com:3478?transport=udp",
        "turn:turn.cloudflare.com:3478?transport=tcp",
        "turns:turn.cloudflare.com:5349?transport=tcp"
      ],
      "username": "test-username",
      "credential": "test-credential"
    }
  ]
}`

func TestCloudflareIssueParsesResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got, want := r.URL.Path, "/test-key/credentials/generate-ice-servers"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			TTL int `json:"ttl"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if parsed.TTL != 3600 {
			t.Errorf("ttl = %d, want 3600", parsed.TTL)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(cfFixtureResponse))
	}))
	t.Cleanup(srv.Close)

	iss, err := NewCloudflareIssuer("test-key", "test-token", time.Hour, withCloudflareEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("NewCloudflareIssuer: %v", err)
	}

	got, err := iss.Issue(t.Context())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	wantURIs := []string{
		"stun:stun.cloudflare.com:3478",
		"stun:stun.cloudflare.com:53",
		"turn:turn.cloudflare.com:3478?transport=udp",
		"turn:turn.cloudflare.com:3478?transport=tcp",
		"turns:turn.cloudflare.com:5349?transport=tcp",
	}
	if !slices.Equal(got.URIs, wantURIs) {
		t.Errorf("URIs = %v, want %v", got.URIs, wantURIs)
	}
	if got.Username != "test-username" {
		t.Errorf("Username = %q, want %q", got.Username, "test-username")
	}
	if got.Credential != "test-credential" {
		t.Errorf("Credential = %q, want %q", got.Credential, "test-credential")
	}
	if got.TTL != 3600 {
		t.Errorf("TTL = %d, want 3600", got.TTL)
	}
}

// TestCloudflareIssueMintsFreshEveryCall guards against re-introducing a
// cache: Cloudflare binds username/credential to a single Allocate, so
// handing the same credential to two peers would silently break the second
// peer's relay.
func TestCloudflareIssueMintsFreshEveryCall(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"iceServers":[{"urls":["turn:t.example:3478"],"username":"u%d","credential":"c%d"}]}`, n, n)
	}))
	t.Cleanup(srv.Close)

	iss, err := NewCloudflareIssuer("k", "t", time.Hour, withCloudflareEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("NewCloudflareIssuer: %v", err)
	}
	first, err := iss.Issue(t.Context())
	if err != nil {
		t.Fatalf("first issue: %v", err)
	}
	second, err := iss.Issue(t.Context())
	if err != nil {
		t.Fatalf("second issue: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
	if first.Credential == second.Credential || first.Username == second.Username {
		t.Errorf("creds should differ across calls: first=%+v second=%+v", first, second)
	}
}

func TestCloudflareErrorOnFailedMint(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	iss, err := NewCloudflareIssuer("k", "t", 1*time.Hour, withCloudflareEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("NewCloudflareIssuer: %v", err)
	}
	if _, err := iss.Issue(t.Context()); err == nil {
		t.Fatal("expected error from failed mint")
	}
}

func TestCloudflareRespectsContext(t *testing.T) {
	t.Parallel()
	// release unblocks the handler before srv.Close runs (cleanup is LIFO,
	// so register srv.Close first and close(release) second).
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	iss, err := NewCloudflareIssuer("k", "t", 1*time.Hour, withCloudflareEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("NewCloudflareIssuer: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = iss.Issue(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestNewCloudflareIssuerValidatesArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		keyID, apiToken  string
		ttl              time.Duration
		wantErrSubstring string
	}{
		{"empty keyID", "", "tok", time.Hour, "keyID"},
		{"empty token", "key", "", time.Hour, "apiToken"},
		{"zero ttl", "key", "tok", 0, "ttl"},
		{"negative ttl", "key", "tok", -time.Minute, "ttl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCloudflareIssuer(tc.keyID, tc.apiToken, tc.ttl)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErrSubstring)
			}
		})
	}
}

func TestCloudflareDecodeURLsAcceptsBothShapes(t *testing.T) {
	t.Parallel()
	// "urls" can be either a string or a []string in WebRTC RTCIceServer.
	body := `{"iceServers":[{"urls":"turn:t.example:3478","username":"u","credential":"c"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)

	iss, err := NewCloudflareIssuer("k", "t", time.Hour, withCloudflareEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("NewCloudflareIssuer: %v", err)
	}
	got, err := iss.Issue(t.Context())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(got.URIs) != 1 || got.URIs[0] != "turn:t.example:3478" {
		t.Errorf("URIs = %v, want [turn:t.example:3478]", got.URIs)
	}
}
