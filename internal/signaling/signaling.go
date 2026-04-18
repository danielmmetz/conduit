// Package signaling implements the conduit rendezvous server.
//
// Phase 1 scope is intentionally tiny: accept a WebSocket connection on /ws
// and send a single hello frame. Slot reservation, pairing, and relay land in
// phase 2.
package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

const ProtocolVersion = "conduit/v1"

type Hello struct {
	Op      string `json:"op"`
	Version string `json:"version"`
}

type Server struct {
	Logger *slog.Logger
}

func (s *Server) HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.Logger.WarnContext(r.Context(), "ws accept failed", slog.Any("err", err), slog.String("remote", r.RemoteAddr))
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	payload, err := json.Marshal(Hello{Op: "hello", Version: ProtocolVersion})
	if err != nil {
		s.Logger.ErrorContext(ctx, "marshal hello failed", slog.Any("err", err))
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		if !errors.Is(err, context.Canceled) {
			s.Logger.WarnContext(ctx, "ws write failed", slog.Any("err", err), slog.String("remote", r.RemoteAddr))
		}
		return
	}
	conn.Close(websocket.StatusNormalClosure, "")
}
