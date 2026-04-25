// conduit-server is the rendezvous / signaling server.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danielmmetz/conduit/internal/ratelimit"
	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/danielmmetz/conduit/internal/turnauth"
	"github.com/danielmmetz/conduit/internal/turnserver"
	"github.com/peterbourgon/ff/v3"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := mainE(ctx, logger); err != nil {
		logger.ErrorContext(ctx, "exiting with error", slog.Any("err", err))
		// Only exit non-zero if our initial context has yet to be canceled.
		// Otherwise it's very likely that the error we're seeing is a result of our attempt at graceful shutdown.
		if ctx.Err() == nil {
			os.Exit(1)
		}
	}
}

func mainE(ctx context.Context, logger *slog.Logger) error {
	fs := flag.NewFlagSet("conduit-server", flag.ContinueOnError)
	var (
		addr          string
		maxSlots      int
		reservePerMin float64
		reserveBurst  int
		joinPerMin    float64
		joinBurst     int
		trustXFF      bool
		turnSecret    string
		turnURIs      []string
		turnEmbed     bool
		turnPublicIP  string
		turnListenUDP string
		turnListenTCP string
	)
	fs.StringVar(&addr, "addr", ":8080", "listen address")
	fs.IntVar(&maxSlots, "max-slots", 2000, "global cap on concurrent reservations (0 disables)")
	fs.Float64Var(&reservePerMin, "reserve-per-min", 30, "reserve attempts per minute per IP (0 disables)")
	fs.IntVar(&reserveBurst, "reserve-burst", 10, "reserve burst size")
	fs.Float64Var(&joinPerMin, "join-per-min", 60, "join attempts per minute per IP (0 disables)")
	fs.IntVar(&joinBurst, "join-burst", 20, "join burst size")
	fs.BoolVar(&trustXFF, "trust-xff", false, "derive source IP from X-Forwarded-For (only when fronted by a trusted proxy)")
	fs.StringVar(&turnSecret, "turn-secret", "", "HMAC secret for TURN credentials (empty disables issuance; with --turn-embed, omitted means generate a random secret for this process only)")
	fs.Func("turn-uri", "TURN URI to advertise (repeat or comma-separate; e.g. turn:turn.example:3478)", func(v string) error {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				turnURIs = append(turnURIs, p)
			}
		}
		return nil
	})
	fs.BoolVar(&turnEmbed, "turn-embed", false, "run an in-process TURN server (requires --turn-public-ip; --turn-secret optional, see --turn-secret help)")
	fs.StringVar(&turnPublicIP, "turn-public-ip", "", "public relay IP advertised to TURN clients when --turn-embed is set")
	fs.StringVar(&turnListenUDP, "turn-listen-udp", ":3478", "UDP listen address for embedded TURN")
	fs.StringVar(&turnListenTCP, "turn-listen-tcp", ":3478", "TCP listen address for embedded TURN")
	if err := ff.Parse(fs, os.Args[1:], ff.WithEnvVarPrefix("CONDUIT_SERVER")); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	issuerURIs := turnURIs
	var relayIP net.IP
	turnSecretAuto := false
	if turnEmbed {
		if turnSecret == "" {
			s, err := randomTurnSecret()
			if err != nil {
				return fmt.Errorf("generating TURN secret: %w", err)
			}
			turnSecret = s
			turnSecretAuto = true
		}
		if turnPublicIP == "" {
			return fmt.Errorf("validating flags: --turn-embed requires --turn-public-ip")
		}
		relayIP = net.ParseIP(turnPublicIP)
		if relayIP == nil {
			return fmt.Errorf("validating flags: --turn-public-ip %q is not a valid IP address", turnPublicIP)
		}
		if len(issuerURIs) == 0 {
			issuerURIs = append(issuerURIs, defaultTurnUDPURI(relayIP, turnListenUDP))
		}
	} else if turnPublicIP != "" {
		return fmt.Errorf("validating flags: --turn-public-ip is only used with --turn-embed")
	}

	var turnSrv *turnserver.Server
	if turnEmbed {
		udpConn, err := net.ListenPacket("udp", turnListenUDP)
		if err != nil {
			return fmt.Errorf("binding TURN udp %q: %w", turnListenUDP, err)
		}
		tcpLn, err := net.Listen("tcp", turnListenTCP)
		if err != nil {
			_ = udpConn.Close()
			return fmt.Errorf("binding TURN tcp %q: %w", turnListenTCP, err)
		}
		turnSrv, err = turnserver.Start(turnserver.Config{
			Secret:      turnSecret,
			RelayIP:     relayIP,
			BindAddress: "0.0.0.0",
			UDPListener: udpConn,
			LogWriter:   os.Stderr,
		}, turnserver.WithTCPListener(tcpLn))
		if err != nil {
			_ = udpConn.Close()
			_ = tcpLn.Close()
			return fmt.Errorf("starting embedded TURN: %w", err)
		}
		defer func() {
			if err := turnSrv.Close(); err != nil {
				logger.ErrorContext(ctx, "closing embedded TURN", slog.Any("err", err))
			}
		}()
		logger.InfoContext(ctx, "embedded TURN listening",
			slog.String("udp", udpConn.LocalAddr().String()),
			slog.String("tcp", tcpLn.Addr().String()),
		)
		if turnSecretAuto {
			logger.InfoContext(ctx, "TURN HMAC secret was auto-generated for this process; set --turn-secret for a stable value across restarts or when splitting TURN to another host")
		}
	}

	opts := []signaling.Option{
		signaling.WithTrustXForwardedFor(trustXFF),
		signaling.WithMaxConcurrentSlots(maxSlots),
	}
	if reservePerMin > 0 {
		opts = append(opts, signaling.WithReserveLimiter(&ratelimit.KeyedLimiter{
			Rate:    rate.Limit(reservePerMin / 60),
			Burst:   reserveBurst,
			IdleTTL: 15 * time.Minute,
		}))
	}
	if joinPerMin > 0 {
		opts = append(opts, signaling.WithJoinLimiter(&ratelimit.KeyedLimiter{
			Rate:    rate.Limit(joinPerMin / 60),
			Burst:   joinBurst,
			IdleTTL: 15 * time.Minute,
		}))
	}
	if turnSecret != "" {
		turnIss, err := turnauth.NewIssuer([]byte(turnSecret), issuerURIs, 10*time.Minute, "conduit", time.Now)
		if err != nil {
			return fmt.Errorf("creating turn issuer: %w", err)
		}
		opts = append(opts, signaling.WithTurnIssuer(turnIss))
	}
	srv := signaling.NewServer(logger, opts...)

	webSub, err := iofs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("embedding web assets: %w", err)
	}
	fileSrv := http.FileServer(http.FS(webSub))
	static := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".wasm") {
			w.Header().Set("Content-Type", "application/wasm")
		}
		fileSrv.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	mux.HandleFunc("GET /healthz", srv.HandleHealthz)
	mux.Handle("GET /", static)

	httpServer := http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	var eg errgroup.Group
	eg.Go(func() error {
		<-serveCtx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		return httpServer.Shutdown(shutdownCtx)
	})

	logger.InfoContext(ctx, "conduit-server listening", slog.String("addr", addr))
	serveErr := httpServer.ListenAndServe()
	cancelServe()
	shutdownErr := eg.Wait()

	if serveErr != nil && serveErr != http.ErrServerClosed {
		return fmt.Errorf("listening and serving: %w", serveErr)
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutting down: %w", shutdownErr)
	}
	return nil
}

// defaultTurnUDPURI builds a single RFC 7065 TURN URI using the same UDP port
// as --turn-listen-udp, for use when --turn-embed is set without explicit --turn-uri.
func defaultTurnUDPURI(ip net.IP, udpListen string) string {
	host := ip.String()
	if ip.To4() == nil {
		host = "[" + host + "]"
	}
	port := 3478
	if a, err := net.ResolveUDPAddr("udp", udpListen); err == nil && a.Port > 0 {
		port = a.Port
	}
	return fmt.Sprintf("turn:%s:%d?transport=udp", host, port)
}

func randomTurnSecret() (string, error) {
	const n = 32
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
