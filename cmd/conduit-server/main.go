// conduit-server is the rendezvous / signaling server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/danielmmetz/conduit/internal/signaling"
	"github.com/peterbourgon/ff/v3"
	"golang.org/x/sync/errgroup"
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
	var addr string
	fs.StringVar(&addr, "addr", ":8080", "listen address")
	if err := ff.Parse(fs, os.Args[1:], ff.WithEnvVarPrefix("CONDUIT_SERVER")); err != nil {
		return err
	}

	srv := signaling.Server{Logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	mux.HandleFunc("GET /healthz", srv.HandleHealthz)

	s := http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	var eg errgroup.Group
	eg.Go(func() error {
		<-ctx.Done()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return s.Shutdown(ctx)
	})

	logger.InfoContext(ctx, "conduit-server listening", slog.String("addr", addr))
	switch err := s.ListenAndServe(); err {
	case http.ErrServerClosed:
		return eg.Wait()
	default:
		return err
	}
}
