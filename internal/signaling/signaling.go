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
	heartbeatInterval  time.Duration
	maxSlot            uint32
	maxConcurrent      int
	reserveLimiter     *ratelimit.KeyedLimiter
	joinLimiter        *ratelimit.KeyedLimiter
	turnIssuer         turnauth.Issuer
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

// WithRelayHeartbeat sets the interval between WebSocket pings sent on each
// peer's connection during the relay phase. Zero disables. A peer that fails
// to acknowledge a ping within one interval has its relay torn down, which
// reclaims the slot when a peer's network silently drops.
func WithRelayHeartbeat(d time.Duration) Option {
	return func(s *Server) { s.heartbeatInterval = d }
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
// mints a fresh credential for each peer at pairing time and includes it
// in that peer's Paired frame. Each peer must get its own credential —
// Cloudflare (and TURN servers in general) bind a credential to a single
// Allocate, so sharing one credential between the two peers in a pair
// silently breaks relay candidate gathering for the second peer.
func WithTurnIssuer(iss turnauth.Issuer) Option {
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
		logger:            logger,
		slotTTL:           10 * time.Minute,
		helloTimeout:      10 * time.Second,
		heartbeatInterval: 30 * time.Second,
		maxSlot:           100_000,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type slot struct {
	id         uint32
	senderConn *websocket.Conn
	persistent bool

	// pair holds the per-pairing state. The sender handler installs a fresh
	// pairing at slot creation and, in persistent mode, replaces it under
	// Server.mu after each pairing's relay teardown so the next receiver's
	// takeSlot lands on a clean object. Reads from join/expiry paths must go
	// through Server.mu.
	pair *pairing
}

// pairing holds the channels and receiver socket for a single sender-to-
// receiver pairing. Persistent slots cycle through a sequence of these
// (one per receiver). Fields are immutable except receiverConn, which
// takeSlot writes once under Server.mu.
type pairing struct {
	receiverConn *websocket.Conn

	// paired is closed under Server.mu by takeSlot once a receiver has
	// attached.
	paired chan struct{}

	// relayStart is closed by the sender handler once both peers have been
	// sent their "paired" control frames. The receiver handler waits on it
	// before beginning its relay direction so that the sender's writes
	// don't race the receiver handler's writes on the sender socket.
	relayStart chan struct{}

	// done is closed once to signal that this pairing is finished, either
	// because one relay direction ended or because a handler bailed out.
	// Both handlers watch done to terminate their peer's read loop.
	done     chan struct{}
	doneOnce sync.Once
}

func newPairing() *pairing {
	return &pairing{
		paired:     make(chan struct{}),
		relayStart: make(chan struct{}),
		done:       make(chan struct{}),
	}
}

// cancel signals that the pairing is finished; safe to call repeatedly.
func (p *pairing) cancel() {
	p.doneOnce.Do(func() { close(p.done) })
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
		// Capacity is checked first so rejected reserves don't drain the
		// per-IP rate-limiter bucket of legitimate users sharing a NAT.
		if s.maxConcurrent > 0 && s.ActiveSlots() >= s.maxConcurrent {
			writeError(r.Context(), conn, wire.ErrCapacity, "server at capacity")
			return
		}
		if s.reserveLimiter != nil && !s.reserveLimiter.Allow(ip) {
			writeError(r.Context(), conn, wire.ErrRateLimited, "reserve rate limit")
			return
		}
		s.handleReserve(r.Context(), logger, conn, hello.Persistent)
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

func (s *Server) handleReserve(ctx context.Context, logger *slog.Logger, conn *websocket.Conn, persistent bool) {
	sl, err := s.newSlot(conn, persistent)
	if err != nil {
		if errors.Is(err, errCapacity) {
			writeError(ctx, conn, wire.ErrCapacity, "server at capacity")
			return
		}
		writeError(ctx, conn, wire.ErrInternal, err.Error())
		return
	}
	defer s.removeSlot(sl.id)

	if err := writeJSON(ctx, conn, wire.Reserved{Op: wire.OpReserved, Slot: sl.id}); err != nil {
		return
	}

	logger = logger.With(slog.Uint64("slot", uint64(sl.id)))
	logger.InfoContext(ctx, "slot reserved", slog.Bool("persistent", persistent))

	// slotCtx bounds every per-slot goroutine — the heartbeat below pings
	// the sender's WS during both relay and the idle gap between pairings,
	// so a sender that drops mid-watch is detected within one ping interval
	// rather than waiting for a fresh receiver to fail at PAKE.
	//
	// Defer ordering matters: slotWG.Wait must run AFTER cancelSlot so the
	// heartbeat goroutine sees ctx-done and exits before we block on Wait.
	// Declaring slotWG.Wait first (= LIFO last) achieves that.
	var slotWG sync.WaitGroup
	defer slotWG.Wait()
	slotCtx, cancelSlot := context.WithCancel(ctx)
	defer cancelSlot()
	slotWG.Go(func() { s.runHeartbeat(slotCtx, conn, cancelSlot) })

	// Long-lived sender pump. coder/websocket closes a conn when its Read
	// context is cancelled, which would kill the slot the moment the first
	// pair ends. Instead, read sender frames with slotCtx (cancelled only
	// when the whole slot is teardown-bound) and route each frame to
	// whichever pairing is currently active. Frames that arrive between
	// pairings — or during the brief gap when a receiver has just left and
	// a new one hasn't joined — are dropped on the floor, which matches
	// what well-behaved senders do (they wait for the next paired envelope
	// before emitting application frames).
	slotWG.Go(func() { s.runSenderPump(slotCtx, sl, cancelSlot, logger) })

	for {
		pair := s.currentPairing(sl)
		if !s.runPairing(slotCtx, logger, sl, pair) {
			return
		}
		if !persistent || slotCtx.Err() != nil {
			return
		}
		s.resetPairing(sl)
	}
}

// runSenderPump reads frames from sl.senderConn for the lifetime of the
// slot and forwards each one to the current pair's receiverConn (when
// any). On Read error the pump cancels the slot context, which unwinds
// handleReserve. The Read uses slotCtx so a per-pair cancellation does
// not also tear down the underlying WebSocket.
func (s *Server) runSenderPump(ctx context.Context, sl *slot, cancelSlot context.CancelFunc, logger *slog.Logger) {
	for {
		typ, data, err := sl.senderConn.Read(ctx)
		if err != nil {
			if !isCleanClose(err) && ctx.Err() == nil {
				logger.InfoContext(ctx, "sender pump ended", slog.Any("err", err))
			}
			cancelSlot()
			return
		}
		s.mu.Lock()
		recv := sl.pair.receiverConn
		s.mu.Unlock()
		if recv == nil {
			continue
		}
		if err := recv.Write(ctx, typ, data); err != nil {
			// The current receiver is gone or stalled; drop the frame.
			// handleJoin's read-side will close the pair imminently.
			continue
		}
	}
}

// runPairing drives one full pair: wait for a receiver, send paired
// envelopes, run the relay, return when the relay ends. Returns true if
// the slot may continue to a next pairing (relay ended cleanly), false if
// the slot is finished (TTL expired, ctx done, or a write to the sender
// failed).
func (s *Server) runPairing(ctx context.Context, logger *slog.Logger, sl *slot, pair *pairing) bool {
	timer := time.NewTimer(s.slotTTL)
	defer timer.Stop()

	select {
	case <-pair.paired:
	case <-timer.C:
		// Remove from the map before notifying the client so that, by the
		// time the sender sees the expired error, a fresh reserve attempt
		// from any IP sees a consistent slot table.
		if s.removeSlotIfUnpaired(sl.id) {
			_ = writeJSON(ctx, sl.senderConn, wire.Error{Op: wire.OpError, Code: wire.ErrExpired})
			logger.InfoContext(ctx, "slot expired before pairing")
			return false
		}
		// Lost the race with a concurrent takeSlot; the channel is closed.
		<-pair.paired
	case <-ctx.Done():
		return false
	}

	defer pair.cancel()

	// From here, both peers are attached. Announce pairing to each end, then
	// release the receiver handler to start relaying. Each peer gets its own
	// freshly-minted TURN credential (when configured): TURN servers bind a
	// credential to one Allocate, so sharing one credential across both peers
	// would break the second peer's relay gathering.
	if err := writeJSON(ctx, sl.senderConn, s.pairedCtl(ctx, logger)); err != nil {
		return false
	}
	if err := writeJSON(ctx, pair.receiverConn, s.pairedCtl(ctx, logger)); err != nil {
		return false
	}
	close(pair.relayStart)

	logger.InfoContext(ctx, "relay started")
	// Sender-to-receiver pumping is the slot-level pump's job; this
	// function just waits for the pair to end (handleJoin signals via
	// pair.done when its read direction errors, or slotCtx is cancelled).
	select {
	case <-pair.done:
	case <-ctx.Done():
		return false
	}
	return true
}

func (s *Server) handleJoin(ctx context.Context, logger *slog.Logger, conn *websocket.Conn, slotID uint32) {
	sl, pair, status := s.takeSlot(slotID, conn)
	switch status {
	case takeNotFound:
		_ = writeJSON(ctx, conn, wire.Error{Op: wire.OpError, Code: wire.ErrSlotNotFound})
		return
	case takeBusy:
		_ = writeJSON(ctx, conn, wire.Error{Op: wire.OpError, Code: wire.ErrSlotBusy, Message: "another receiver is currently paired with this slot"})
		return
	}
	defer pair.cancel()

	logger = logger.With(slog.Uint64("slot", uint64(slotID)))

	select {
	case <-pair.relayStart:
	case <-pair.done:
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
		case <-pair.done:
			cancelRelay()
		case <-relayCtx.Done():
		}
	})
	wg.Go(func() { s.runHeartbeat(relayCtx, conn, cancelRelay) })

	if err := relay(relayCtx, conn, sl.senderConn); err != nil && !isCleanClose(err) && relayCtx.Err() == nil {
		logger.InfoContext(ctx, "receiver→sender relay ended", slog.Any("err", err))
	}
}

// errCapacity signals that the global concurrent-slot cap is reached.
var errCapacity = fmt.Errorf("server at capacity")

// newSlot allocates a fresh slot with one initial pairing object and
// registers the sender connection.
func (s *Server) newSlot(senderConn *websocket.Conn, persistent bool) (*slot, error) {
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
			persistent: persistent,
			pair:       newPairing(),
		}
		s.slots[id] = sl
		return sl, nil
	}
	return nil, fmt.Errorf("slot table exhausted")
}

// takeStatus reports the outcome of a takeSlot attempt: ok means the
// receiver was attached, notFound means the slot does not exist, busy means
// the slot exists but already has a receiver attached for the current
// pairing (only reachable on persistent slots while a previous pairing has
// not finished tearing down).
type takeStatus int

const (
	takeOK takeStatus = iota
	takeNotFound
	takeBusy
)

// takeSlot atomically claims the current pairing's receiver position. On
// success it populates pair.receiverConn and closes pair.paired under the
// lock; the returned pairing pointer is the snapshot the caller's relay
// loop should use.
func (s *Server) takeSlot(id uint32, recvConn *websocket.Conn) (*slot, *pairing, takeStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[id]
	if !ok {
		return nil, nil, takeNotFound
	}
	if sl.pair.receiverConn != nil {
		return nil, nil, takeBusy
	}
	sl.pair.receiverConn = recvConn
	pair := sl.pair
	close(pair.paired)
	return sl, pair, takeOK
}

// resetPairing installs a fresh pairing on a persistent slot after the
// previous pairing's relay has been fully torn down. Callers must hold no
// other references to the old pairing — its goroutines must already have
// exited — since this is what the next takeSlot's "is the pairing
// available" check observes.
func (s *Server) resetPairing(sl *slot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl.pair = newPairing()
}

// currentPairing returns the slot's current pairing under the lock so the
// sender's loop sees a consistent snapshot before each iteration.
func (s *Server) currentPairing(sl *slot) *pairing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sl.pair
}

// removeSlotIfUnpaired atomically deletes id iff its current pairing has
// no receiver attached, reporting whether it did so. Callers use the
// return value to distinguish "really expired" from "the receiver just
// beat us".
func (s *Server) removeSlotIfUnpaired(id uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sl, ok := s.slots[id]
	if !ok || sl.pair.receiverConn != nil {
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

// runHeartbeat pings conn every heartbeatInterval until ctx is done. A failed
// ping (peer gone or network silently dropped) cancels the relay so the slot
// is reclaimed instead of pinning a goroutine until TCP keepalive notices —
// which can be hours on a default Linux. coder/websocket serializes Ping with
// data writes via writeFrameMu, so this is safe to run alongside relay's
// cross-handler writes to conn.
func (s *Server) runHeartbeat(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	if s.heartbeatInterval <= 0 {
		return
	}
	t := time.NewTicker(s.heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, pcancel := context.WithTimeout(ctx, s.heartbeatInterval)
			err := conn.Ping(pingCtx)
			pcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
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

func (s *Server) pairedCtl(ctx context.Context, logger *slog.Logger) wire.Paired {
	return wire.Paired{Op: wire.OpPaired, TURN: s.issueTURN(ctx, logger)}
}

// issueTURN mints credentials for inclusion in a Paired control frame.
// Returns nil if no issuer is configured or if issuance failed; the caller
// treats either as "no TURN advertised this round" so a transient upstream
// blip doesn't break the whole signaling exchange — peers can still pair
// directly when both sides have non-symmetric NAT.
func (s *Server) issueTURN(ctx context.Context, logger *slog.Logger) *wire.TurnCreds {
	if s.turnIssuer == nil {
		return nil
	}
	creds, err := s.turnIssuer.Issue(ctx)
	if err != nil {
		logger.WarnContext(ctx, "issuing turn credentials", slog.Any("err", err))
		return nil
	}
	return &wire.TurnCreds{
		URIs:       creds.URIs,
		Username:   creds.Username,
		Credential: creds.Credential,
		TTL:        creds.TTL,
	}
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
