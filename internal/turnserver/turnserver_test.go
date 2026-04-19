package turnserver_test

import (
	"net"
	"testing"
	"time"

	"github.com/danielmmetz/conduit/internal/turnauth"
	"github.com/danielmmetz/conduit/internal/turnserver"
	"github.com/pion/turn/v4"
)

// TestAllocate stands up a UDP-only loopback TURN server, mints credentials
// through internal/turnauth (the same issuer the signaling server uses), and
// confirms pion's TURN client can complete an ALLOCATE. That end-to-end handshake
// is the cheapest thing that proves the credential format matches.
func TestAllocate(t *testing.T) {
	t.Parallel()
	secret := []byte("phase5-turn-secret")
	udpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	serverAddr := udpConn.LocalAddr().(*net.UDPAddr)

	srv, err := turnserver.Start(turnserver.Config{
		Secret:      string(secret),
		RelayIP:     net.ParseIP("127.0.0.1"),
		BindAddress: "127.0.0.1",
		UDPListener: udpConn,
		LogWriter:   t.Output(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})

	iss, err := turnauth.NewIssuer(secret, nil, 5*time.Minute, "conduit", time.Now)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	creds := iss.Issue()

	clientConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen client udp: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr.String(),
		TURNServerAddr: serverAddr.String(),
		Username:       creds.Username,
		Password:       creds.Credential,
		Realm:          turnserver.Realm,
		Conn:           clientConn,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		t.Fatalf("client listen: %v", err)
	}

	relay, err := client.Allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	defer relay.Close()
	if got := relay.LocalAddr().String(); got == "" {
		t.Fatalf("empty relay addr")
	}
}

// TestRejectsBadCredential ensures the server authenticates with its shared
// secret rather than accepting any well-formed long-term username.
func TestRejectsBadCredential(t *testing.T) {
	t.Parallel()
	udpConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	serverAddr := udpConn.LocalAddr().(*net.UDPAddr)

	srv, err := turnserver.Start(turnserver.Config{
		Secret:      "correct-secret",
		RelayIP:     net.ParseIP("127.0.0.1"),
		BindAddress: "127.0.0.1",
		UDPListener: udpConn,
		LogWriter:   t.Output(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	wrong, err := turnauth.NewIssuer([]byte("attacker-secret"), nil, 5*time.Minute, "conduit", time.Now)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	creds := wrong.Issue()

	clientConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen client udp: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close() })

	client, err := turn.NewClient(&turn.ClientConfig{
		STUNServerAddr: serverAddr.String(),
		TURNServerAddr: serverAddr.String(),
		Username:       creds.Username,
		Password:       creds.Credential,
		Realm:          turnserver.Realm,
		Conn:           clientConn,
		RTO:            200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		t.Fatalf("client listen: %v", err)
	}

	if _, err := client.Allocate(); err == nil {
		t.Fatal("allocate with wrong secret should have failed")
	}
}
