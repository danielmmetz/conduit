// Package signaling implements the conduit rendezvous server.
//
// Phase 2 scope: slot reservation, peer pairing, and verbatim message relay
// between two WebSocket peers. No content is parsed after the control
// handshake; the server never sees plaintext payloads.
package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/ratelimit"
	"github.com/danielmmetz/conduit/internal/turnauth"
	"github.com/danielmmetz/conduit/internal/wire"
)

const newSlotAttempts = 100

// Server is the rendezvous HTTP handler. Construct via NewServer.
type Server struct {
	logger             *slog.Logger
	slotTTL            time.Duration
	helloTimeout       time.Duration
	maxSlot            uint32
	maxConcurrent      int
	reserveLimiter     *ratelimit.KeyedLimiter
	joinLimiter        *ratelimit.KeyedLimiter
	turnIssuer         *turnauth.Issuer
	trustXForwardedFor bool

	mu    sync.Mutex
	slots map[uint32]*slot
}

// Option configures a Server in NewServer.
type Option func(*Server)

// WithSlotTTL overrides the default slot reservation timeout.
func WithSlotTTL(d time.Duration) Option {
	return func(s *Server) { s.slotTTL = d }
}

// WithHelloTimeout overrides the default first-frame timeout.
func WithHelloTimeout(d time.Duration) Option {
	return func(s *Server) { s.helloTimeout = d }
}

// WithMaxSlot overrides the exclusive upper bound of slot IDs.
func WithMaxSlot(n uint32) Option {
	return func(s *Server) { s.maxSlot = n }
}

// WithMaxConcurrentSlots caps live reservations globally. Zero disables the
// cap. When exceeded, new reserves are rejected with ErrCapacity rather than
// tying up memory and filing descriptors.
func WithMaxConcurrentSlots(n int) Option {
	return func(s *Server) { s.maxConcurrent = n }
}

// WithReserveLimiter installs a per-IP rate limiter for reserve ops.
func WithReserveLimiter(l *ratelimit.KeyedLimiter) Option {
	return func(s *Server) { s.reserveLimiter = l }
}

// WithJoinLimiter installs a per-IP rate limiter for join ops.
func WithJoinLimiter(l *ratelimit.KeyedLimiter) Option {
	return func(s *Server) { s.joinLimiter = l }
}

// WithTurnIssuer installs a TURN credential issuer. When set, the server
// includes fresh credentials in every Reserved frame.
func WithTurnIssuer(iss *turnauth.Issuer) Option {
	return func(s *Server) { s.turnIssuer = iss }
}

// WithTrustXForwardedFor, when true, derives the source IP for rate limiting
// from the last X-Forwarded-For entry. Enable only when a trusted proxy
// terminates TLS in front of this server.
func WithTrustXForwardedFor(trust bool) Option {
	return func(s *Server) { s.trustXForwardedFor = trust }
}

// NewServer builds a rendezvous server. Logger is required; all other fields
// get their defaults and can be overridden via Options.
func NewServer(logger *slog.Logger, opts ...Option) *Server {
	s := &Server{
		logger:       logger,
		slotTTL:      10 * time.Minute,
		helloTimeout: 10 * time.Second,
		maxSlot:      100_000,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type slot struct {
	id           uint32
	senderConn   *websocket.Conn
	receiverConn *websocket.Conn

	// paired is closed under s.mu by takeSlot once a receiver has attached.
	paired chan struct{}

	// relayStart is closed by the sender handler once both peers have been
	// sent their "paired" control frames. The receiver handler waits on it
	// before beginning its relay direction so that the sender's writes
	// don't race the receiver handler's writes on the sender socket.
	relayStart chan struct{}

	// done is closed once to signal that the slot is finished, either because
	// one relay direction ended or because a handler bailed out. Both
	// handlers watch done to terminate their peer's read loop.
	done     chan struct{}
	doneOnce sync.Once
}

// cancel signals that the slot is finished; it is safe to call repeatedly.
func (sl *slot) cancel() {
	sl.doneOnce.Do(func() { close(sl.done) })
}

// HandleHealthz reports liveness.
func (s *Server) HandleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// HandleWS accepts a WebSocket connection and dispatches on the client's
// first control frame.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	ip := s.sourceIP(r)
	logger := s.logger.With(slog.String("remote", ip))

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		logger.WarnContext(r.Context(), "ws accept failed", slog.Any("err", err))
		return
	}
	defer conn.CloseNow()

	helloCtx, cancel := context.WithTimeout(r.Context(), s.helloTimeout)
	_, data, err := conn.Read(helloCtx)
	cancel()
	if err != nil {
		logger.DebugContext(r.Context(), "hello read failed", slog.Any("err", err))
		return
	}

	var hello wire.ClientHello
	if err := json.Unmarshal(data, &hello); err != nil {
		writeError(r.Context(), conn, wire.ErrBadRequest, "invalid json")
		return
	}

	switch hello.Op {
	case wire.OpReserve:
		if s.reserveLimiter != nil && !s.reserveLimiter.Allow(ip) {
			writeError(r.Context(), conn, wire.ErrRateLimited, "reserve rate limit")
			return
		}
		s.handleReserve(r.Context(), logger, conn)
	case wire.OpJoin:
		if s.joinLimiter != nil && !s.joinLimiter.Allow(ip) {
			writeError(r.Context(), conn, wire.ErrRateLimited, "join rate limit")
			return
		}
		s.handleJoin(r.Context(), logger, conn, hello.Slot)
	default:
		writeError(r.Context(), conn, wire.ErrBadRequest, "unknown op")
	}
}

func (s *Server) handleReserve(ctx context.Context, logger *slog.Logger, conn *websocket.Conn) {
	sl, err := s.newSlot(conn)
	if err != nil {
		if errors.Is(err, errCapacity) {
			writeError(ctx, conn, wire.ErrCapacity, "server at capacity")
			return
		}
		writeError(ctx, conn, wire.ErrInternal, err.Error())
		return
	}
	defer sl.cancel()
	defer s.removeSlot(sl.id)

	reserved := wire.Reserved{Op: wire.OpReserved, Slot: sl.id}
	if s.turnIssuer != nil {
		creds := s.turnIssuer.Issue()
		reserved.TURN = &wire.TurnCreds{
			URIs:       creds.URIs,
			Username:   creds.Username,
			Credential: creds.Credential,
			TTL:        creds.TTL,
		}
	}
	if err := writeJSON(ctx, conn, reserved); err != nil {
		return
	}

	logger = logger.With(slog.Uint64("slot", uint64(sl.id)))
	logger.InfoContext(ctx, "slot reserved")

	timer := time.NewTimer(s.slotTTL)
	defer timer.Stop()

	select {
	case <-sl.paired:
	case <-timer.C:
		// Remove from the map before notifying the client so that, by the
		// time the sender sees the expired error, a fresh reserve attempt
		// from any IP sees a consistent slot table.
		if s.removeSlotIfUnpaired(sl.id) {
			_ = writeJSON(ctx, conn, wire.Error{Op: wire.OpError, Code: wire.ErrExpired})
			logger.InfoContext(ctx, "slot expired before pairing")
			return
		}
		// Lost the race with a concurrent takeSlot; the channel is closed.
		<-sl.paired
	case <-ctx.Done():
		return
	}

	// From here, both peers are attached. Announce pairing to each end, then
	// release the receiver handler to start relaying. Fresh TURN credentials
	// (when configured) are included so both peers can gather relay candidates.
	if err := writeJSON(ctx, conn, s.pairedCtl()); err != nil {
		return
	}
	if err := writeJSON(ctx, sl.receiverConn, s.pairedCtl()); err != nil {
		return
	}
	close(sl.relayStart)

	logger.InfoContext(ctx, "relay started")
	relayCtx, cancelRelay := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancelRelay()
	wg.Go(func() {
		select {
		case <-sl.done:
			cancelRelay()
		case <-relayCtx.Done():
		}
	})

	if err := relay(relayCtx, conn, sl.receiverConn); err != nil && !isCleanClose(err) && relayCtx.Err() == nil {
		logger.InfoContext(ctx, "sender→receiver relay ended", slog.Any("err", err))
	}
}

func (s *Server) handleJoin(ctx context.Context, logger *slog.Logger, conn *websocket.Conn, slotID uint32) {
	sl, ok := s.takeSlot(slotID, conn)
	if !ok {
		_ = writeJSON(ctx, conn, wire.Error{Op: wire.OpError, Code: wire.ErrSlotNotFound})
		return
	}
	defer sl.cancel()

	logger = logger.With(slog.Uint64("slot", uint64(slotID)))

	select {
	case <-sl.relayStart:
	case <-sl.done:
		return
	case <-ctx.Done():
		return
	}

	relayCtx, cancelRelay := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancelRelay()
	wg.Go(func() {
		select {
		case <-sl.done:
			cancelRelay()
		case <-relayCtx.Done():
		}
	})

	if err := relay(relayCtx, conn, sl.senderConn); err != nil && !isCleanClose(err) && relayCtx.Err() == nil {
		logger.InfoContext(ctx, "receiver→sender relay ended", slog.Any("err", err))
	}
}

// errCapacity signals that the global concurrent-slot cap is reached.
var errCapacity = fmt.Errorf("server at capacity")

// newSlot allocates a fresh slot and registers the sender connection.
func (s *Server) newSlot(senderConn *websocket.Conn) (*slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = make(map[uint32]*slot)
	}
	if s.maxConcurrent > 0 && len(s.slots) >= s.maxConcurrent {
		return nil, errCapacity
	}
	for range newSlotAttempts {
		id := rand.Uint32N(s.maxSlot-1) + 1
		if _, exists := s.slots[id]; exists {
			continue
		}
		sl := &slot{
			id:         id,
			senderConn: senderConn,
			paired:     make(chan struct{}),
			relayStart: make(chan struct{}),
			done:       make(chan struct{}),
		}
		s.slots[id] = sl
		return sl, nil
	}
	return nil, fmt.Errorf("slot table exhausted")
}

// takeSlot atomically claims an unpaired slot for the receiver. On success it
// populates the receiver conn and closes the paired channel under the lock.
func (s *Server) takeSlot(id uint32, recvConn *websocket.Conn) (*slot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[id]
	if !ok || sl.receiverConn != nil {
		return nil, false
	}
	sl.receiverConn = recvConn
	close(sl.paired)
	return sl, true
}

// removeSlotIfUnpaired atomically deletes id iff it has not been paired,
// reporting whether it did so. Callers use the return value to distinguish
// "really expired" from "the receiver just beat us".
func (s *Server) removeSlotIfUnpaired(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[id]
	if !ok || sl.receiverConn != nil {
		return false
	}
	delete(s.slots, id)
	return true
}

// removeSlot deletes id from the slot map if present.
func (s *Server) removeSlot(id uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, id)
}

// ActiveSlots returns the current count of live slots. Intended for tests
// and metrics.
func (s *Server) ActiveSlots() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.slots)
}

func (s *Server) sourceIP(r *http.Request) string {
	if s.trustXForwardedFor {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			last := strings.TrimSpace(parts[len(parts)-1])
			if last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// relay forwards frames from src to dst verbatim until the context is canceled
// or either socket errors.
func relay(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			return fmt.Errorf("reading relay frame: %w", err)
		}
		if err := dst.Write(ctx, typ, data); err != nil {
			return fmt.Errorf("writing relay frame: %w", err)
		}
	}
}

func (s *Server) pairedCtl() wire.Paired {
	msg := wire.Paired{Op: wire.OpPaired}
	if s.turnIssuer != nil {
		creds := s.turnIssuer.Issue()
		msg.TURN = &wire.TurnCreds{
			URIs:       creds.URIs,
			Username:   creds.Username,
			Credential: creds.Credential,
			TTL:        creds.TTL,
		}
	}
	return msg
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling control frame: %w", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return fmt.Errorf("writing control frame: %w", err)
	}
	return nil
}

func writeError(ctx context.Context, conn *websocket.Conn, code, message string) {
	_ = writeJSON(ctx, conn, wire.Error{Op: wire.OpError, Code: code, Message: message})
}

func isCleanClose(err error) bool {
	if err == nil {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway, websocket.StatusNoStatusRcvd:
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, context.Canceled)
}
