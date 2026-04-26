// conduit-server is the rendezvous / signaling server.
package main

import (
	"context"
	"flag"
	"fmt"
	iofs "io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danielmmetz/conduit/internal/ratelimit"
	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/danielmmetz/conduit/internal/turnauth"
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
		addr             string
		maxSlots         int
		reservePerMin    float64
		reserveBurst     int
		joinPerMin       float64
		joinBurst        int
		trustXFF         bool
		cfTurnKeyID      string
		cfTurnAPIToken   string
		cfTurnTTLSeconds int
	)
	fs.StringVar(&addr, "addr", ":8080", "listen address")
	fs.IntVar(&maxSlots, "max-slots", 2000, "global cap on concurrent reservations (0 disables)")
	fs.Float64Var(&reservePerMin, "reserve-per-min", 30, "reserve attempts per minute per IP (0 disables)")
	fs.IntVar(&reserveBurst, "reserve-burst", 10, "reserve burst size")
	fs.Float64Var(&joinPerMin, "join-per-min", 60, "join attempts per minute per IP (0 disables)")
	fs.IntVar(&joinBurst, "join-burst", 20, "join burst size")
	fs.BoolVar(&trustXFF, "trust-xff", false, "derive source IP from X-Forwarded-For (only when fronted by a trusted proxy)")
	fs.StringVar(&cfTurnKeyID, "cloudflare-turn-key-id", "", "Cloudflare Realtime TURN key ID (paired with --cloudflare-turn-api-token)")
	fs.StringVar(&cfTurnAPIToken, "cloudflare-turn-api-token", "", "Cloudflare Realtime TURN per-key API token (paired with --cloudflare-turn-key-id)")
	fs.IntVar(&cfTurnTTLSeconds, "cloudflare-turn-ttl-seconds", 3600, "TTL requested for each Cloudflare-issued TURN credential")
	if err := ff.Parse(fs, os.Args[1:], ff.WithEnvVarPrefix("CONDUIT_SERVER")); err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	cfTurnEnabled := cfTurnKeyID != "" || cfTurnAPIToken != ""
	if cfTurnEnabled {
		if cfTurnKeyID == "" || cfTurnAPIToken == "" {
			return fmt.Errorf("validating flags: --cloudflare-turn-key-id and --cloudflare-turn-api-token must be set together")
		}
		if cfTurnTTLSeconds <= 0 {
			return fmt.Errorf("validating flags: --cloudflare-turn-ttl-seconds must be positive, got %d", cfTurnTTLSeconds)
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
	if cfTurnEnabled {
		cfIss, err := turnauth.NewCloudflareIssuer(cfTurnKeyID, cfTurnAPIToken, time.Duration(cfTurnTTLSeconds)*time.Second)
		if err != nil {
			return fmt.Errorf("creating cloudflare turn issuer: %w", err)
		}
		opts = append(opts, signaling.WithTurnIssuer(cfIss))
		logger.InfoContext(ctx, "using Cloudflare Realtime TURN", slog.String("key_id", cfTurnKeyID), slog.Int("ttl_seconds", cfTurnTTLSeconds))
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
