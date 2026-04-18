package signaling_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/signaling"
)

func newTestMux(t *testing.T) *http.ServeMux {
	t.Helper()
	srv := signaling.Server{Logger: slog.New(slog.NewTextHandler(t.Output(), nil))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", srv.HandleWS)
	mux.HandleFunc("GET /healthz", srv.HandleHealthz)
	return mux
}

func TestHelloWS(t *testing.T) {
	ts := httptest.NewServer(newTestMux(t))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("message type = %v, want text", typ)
	}

	var hello signaling.Hello
	if err := json.Unmarshal(data, &hello); err != nil {
		t.Fatalf("unmarshal: %v (payload=%q)", err, data)
	}
	if hello.Op != "hello" {
		t.Errorf("op = %q, want %q", hello.Op, "hello")
	}
	if hello.Version != signaling.ProtocolVersion {
		t.Errorf("version = %q, want %q", hello.Version, signaling.ProtocolVersion)
	}

	// Server should close cleanly after the hello frame.
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatalf("expected EOF / close after hello, got nil")
	} else if !isCleanClose(err) {
		t.Fatalf("unexpected read error after hello: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	ts := httptest.NewServer(newTestMux(t))
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ok") {
		t.Errorf("body = %q, want contains %q", body, "ok")
	}
}

func isCleanClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusNoStatusRcvd || err == io.EOF
}
