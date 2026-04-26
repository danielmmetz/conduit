// Package turnauth mints TURN credentials for the conduit signaling server.
//
// The current implementation lives in cloudflare.go and mints short-lived
// credentials from Cloudflare Realtime TURN. The Issuer interface is the
// extension point if another provider ever needs to be plugged in.
package turnauth

import "context"

// Creds are the fields a client needs to hand to a TURN server or WebRTC stack.
type Creds struct {
	URIs       []string `json:"uris"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
	TTL        int      `json:"ttl"`
}

// Issuer mints TURN credentials for a peer session. Implementations may block
// on the network and must respect ctx.
type Issuer interface {
	Issue(ctx context.Context) (Creds, error)
}
