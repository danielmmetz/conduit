// conduit-turn is a standalone pion/turn process for deployments that want TURN
// on a separate host or port from conduit-server. The same TURN stack is also
// available in-process via conduit-server --turn-embed.
//
// It authenticates ephemeral credentials minted by the signaling server via
// internal/turnauth (RFC 8489 long-term/ REST-style: username is
// "<expiry>:<prefix>"; credential is base64 HMAC-SHA1 over the username).
//
// Listens on :3478 UDP+TCP (RFC 8489 defaults). TLS on 5349 is a Caddy concern
// (phase 8).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/danielmmetz/conduit/internal/turnserver"
	"github.com/peterbourgon/ff/v3"
)

// Fixed deployment choices. Both sides of the TURN protocol are ours, so these
// do not need to be tunable: port 3478 is the RFC 8489 default and 0.0.0.0
// binds every interface.
const (
	udpAddr     = ":3478"
	tcpAddr     = ":3478"
	bindAddress = "0.0.0.0"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := mainE(ctx, logger); err != nil {
		logger.ErrorContext(ctx, "exiting with error", slog.Any("err", err))
		if ctx.Err() == nil {
			os.Exit(1)
		}
	}
}

func mainE(ctx context.Context, logger *slog.Logger) error {
	fs := flag.NewFlagSet("conduit-turn", flag.ContinueOnError)
	var (
		publicIP string
		secret   string
	)
	fs.StringVar(&publicIP, "public-ip", "", "public IP advertised to clients as the relay address (required)")
	fs.StringVar(&secret, "secret", "", "HMAC secret; must match conduit-server --turn-secret (required)")
	if err := ff.Parse(fs, os.Args[1:], ff.WithEnvVarPrefix("CONDUIT_TURN")); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}
	if publicIP == "" {
		return fmt.Errorf("validating flags: --public-ip is required")
	}
	if secret == "" {
		return fmt.Errorf("validating flags: --secret is required")
	}
	relayIP := net.ParseIP(publicIP)
	if relayIP == nil {
		return fmt.Errorf("validating flags: --public-ip %q is not a valid IP", publicIP)
	}

	udpConn, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("binding udp %q: %w", udpAddr, err)
	}
	tcpLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		_ = udpConn.Close()
		return fmt.Errorf("binding tcp %q: %w", tcpAddr, err)
	}
	logger.InfoContext(ctx, "turn listening",
		slog.String("udp", udpConn.LocalAddr().String()),
		slog.String("tcp", tcpLn.Addr().String()),
	)

	srv, err := turnserver.Start(turnserver.Config{
		Secret:      secret,
		RelayIP:     relayIP,
		BindAddress: bindAddress,
		UDPListener: udpConn,
		LogWriter:   os.Stderr,
	}, turnserver.WithTCPListener(tcpLn))
	if err != nil {
		_ = udpConn.Close()
		_ = tcpLn.Close()
		return fmt.Errorf("starting turn server: %w", err)
	}

	<-ctx.Done()
	if err := srv.Close(); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}
