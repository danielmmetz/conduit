package turnauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// cloudflareEndpoint is the production Cloudflare Realtime TURN credential
// endpoint. Tests override it via withCloudflareEndpoint.
const cloudflareEndpoint = "https://rtc.live.cloudflare.com/v1/turn/keys"

// CloudflareIssuer mints short-lived credentials from Cloudflare Realtime
// TURN. Each Issue call mints fresh: Cloudflare expects one credential per
// peer (per Allocate), so a cache that handed the same credential to multiple
// callers would silently break TURN allocation for everyone but the first.
type CloudflareIssuer struct {
	keyID    string
	apiToken string
	ttl      time.Duration
	endpoint string
	client   *http.Client
}

// cloudflareOption configures a CloudflareIssuer in NewCloudflareIssuer.
// Test-only; no production caller needs the seams these expose.
type cloudflareOption func(*CloudflareIssuer)

func withCloudflareEndpoint(u string) cloudflareOption {
	return func(c *CloudflareIssuer) { c.endpoint = u }
}

// NewCloudflareIssuer builds a CloudflareIssuer for the given TURN key ID
// and per-key API token (both obtained from the Cloudflare Calls dashboard
// or API). ttl is the lifetime requested per credential.
func NewCloudflareIssuer(keyID, apiToken string, ttl time.Duration, opts ...cloudflareOption) (*CloudflareIssuer, error) {
	if keyID == "" {
		return nil, fmt.Errorf("validating: cloudflare keyID must be non-empty")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("validating: cloudflare apiToken must be non-empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("validating: ttl must be positive, received %v", ttl)
	}
	c := &CloudflareIssuer{
		keyID:    keyID,
		apiToken: apiToken,
		ttl:      ttl,
		endpoint: cloudflareEndpoint,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Issue mints a fresh credential from Cloudflare. Callers must not share the
// returned Creds across peers — Cloudflare binds username/credential to a
// single Allocate, so reuse causes the second peer's relay allocation to be
// silently rejected.
func (c *CloudflareIssuer) Issue(ctx context.Context) (Creds, error) {
	body, err := json.Marshal(map[string]any{"ttl": int(c.ttl / time.Second)})
	if err != nil {
		return Creds{}, fmt.Errorf("marshaling request: %w", err)
	}
	url := fmt.Sprintf("%s/%s/credentials/generate-ice-servers", c.endpoint, c.keyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Creds{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return Creds{}, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Creds{}, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return Creds{}, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, truncate(string(respBody), 256))
	}

	var parsed cloudflareResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return Creds{}, fmt.Errorf("decoding response: %w", err)
	}

	var (
		uris       []string
		username   string
		credential string
	)
	for _, srv := range parsed.IceServers {
		urls, err := decodeURLs(srv.URLs)
		if err != nil {
			return Creds{}, fmt.Errorf("decoding iceServer urls: %w", err)
		}
		uris = append(uris, urls...)
		// Cloudflare's TURN entry is the one with credentials. STUN entries
		// have neither and don't override.
		if srv.Username != "" && srv.Credential != "" {
			username = srv.Username
			credential = srv.Credential
		}
	}
	if username == "" || credential == "" {
		return Creds{}, fmt.Errorf("response contained no TURN credential")
	}
	if len(uris) == 0 {
		return Creds{}, fmt.Errorf("response contained no URIs")
	}
	return Creds{
		URIs:       uris,
		Username:   username,
		Credential: credential,
		TTL:        int(c.ttl / time.Second),
	}, nil
}

// cloudflareResponse mirrors the relevant subset of the Cloudflare TURN
// generate-ice-servers response. The endpoint returns one entry per
// distinct credential set (typically one STUN entry without auth and one
// TURN entry with auth); we flatten them into a single Creds.
type cloudflareResponse struct {
	IceServers []cloudflareIceServer `json:"iceServers"`
}

type cloudflareIceServer struct {
	URLs       json.RawMessage `json:"urls"`
	Username   string          `json:"username,omitempty"`
	Credential string          `json:"credential,omitempty"`
}

// decodeURLs accepts both the WebRTC-style "urls" field forms: a single
// string or an array of strings.
func decodeURLs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("urls is neither string nor []string: %w", err)
	}
	return []string{single}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
