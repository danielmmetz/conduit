// Package turnserver wraps github.com/pion/turn/v4 with the credential format
// issued by internal/turnauth, so a conduit-server and a conduit-turn sharing
// the same HMAC secret interoperate out of the box.
//
// The username the client presents is "<unix_expiry>:<prefix>"; the credential
// is the base64 HMAC-SHA1 of the username keyed by the shared secret. That is
// the ephemeral scheme described by draft-uberti-behave-turn-rest-00, which
// pion implements via turn.LongTermTURNRESTAuthHandler.
package turnserver

import (
	"fmt"
	"io"
	"net"

	"github.com/pion/logging"
	"github.com/pion/turn/v4"
)

// Realm is the fixed TURN realm used across conduit components. Pion mixes it
// into the MD5 key derivation during authentication; clients learn it from the
// server's WWW-Authenticate challenge rather than via config, so there is no
// case where another value would be usefully externally visible.
const Realm = "conduit"

// Config describes a TURN server. Every field is required; optional behavior
// is exposed via Option values passed to Start.
type Config struct {
	// Secret is the shared HMAC secret. It must match the one configured on
	// the conduit-server that issues credentials (internal/turnauth).
	Secret string

	// RelayIP is the public IP advertised to peers as the relay address. For
	// loopback tests, 127.0.0.1.
	RelayIP net.IP

	// BindAddress is the interface address the relay sockets bind to. Use
	// "0.0.0.0" to listen on every interface.
	BindAddress string

	// UDPListener is the UDP socket pion serves TURN requests on. Callers open
	// it so they can pick ephemeral ports in tests or add their own listener
	// instrumentation.
	UDPListener net.PacketConn

	// LogWriter receives pion's internal logs.
	LogWriter io.Writer
}

// Option configures optional behavior on top of Config.
type Option func(*options)

type options struct {
	tcpListener net.Listener
}

// WithTCPListener adds a TCP listener so TURN-over-TCP clients are also served.
func WithTCPListener(ln net.Listener) Option {
	return func(o *options) { o.tcpListener = ln }
}

// Server is a running TURN server. Close it to release ports.
type Server struct {
	inner *turn.Server
}

// Start builds and starts a TURN server from cfg. All Config fields must be
// populated; optional behavior is opt-in through Option values.
func Start(cfg Config, opts ...Option) (*Server, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	loggerFactory := logging.NewDefaultLoggerFactory()
	loggerFactory.Writer = cfg.LogWriter
	authLogger := loggerFactory.NewLogger("conduit-turn-auth")

	sc := turn.ServerConfig{
		Realm:         Realm,
		AuthHandler:   turn.LongTermTURNRESTAuthHandler(cfg.Secret, authLogger),
		LoggerFactory: loggerFactory,
		PacketConnConfigs: []turn.PacketConnConfig{{
			PacketConn: cfg.UDPListener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: cfg.RelayIP,
				Address:      cfg.BindAddress,
			},
		}},
	}
	if o.tcpListener != nil {
		sc.ListenerConfigs = append(sc.ListenerConfigs, turn.ListenerConfig{
			Listener: o.tcpListener,
			RelayAddressGenerator: &turn.RelayAddressGeneratorStatic{
				RelayAddress: cfg.RelayIP,
				Address:      cfg.BindAddress,
			},
		})
	}
	srv, err := turn.NewServer(sc)
	if err != nil {
		return nil, fmt.Errorf("starting turn server: %w", err)
	}
	return &Server{inner: srv}, nil
}

// Close stops the server and closes its listeners.
func (s *Server) Close() error {
	if err := s.inner.Close(); err != nil {
		return fmt.Errorf("closing turn server: %w", err)
	}
	return nil
}
