package rtc

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/webrtc/v4"
)

// trickleManager owns the trickle ICE machinery for one PeerConnection.
// It captures local candidates fired from pion's gatherer, forwards them
// over sig once the SDP exchange is complete, runs the single goroutine
// permitted to read from sig (so trickled candidates, end-of-candidates,
// and the final teardown signal all share one reader), and provides the
// bidirectional teardown handshake.
//
// Lifecycle:
//
//	tm := newTrickleManager(ctx, pc, sig, key, logger)
//	defer tm.close()
//	... exchangeSDP ...
//	tm.start(ctx)
//	... application phase ...
//	tm.exchangeTeardown(ctx)
//
// newTrickleManager registers OnICECandidate immediately because pion can
// fire candidates from inside SetLocalDescription (gathering starts there
// and the gatherer goroutine may invoke the handler before the call
// returns). Until start runs, candidates are queued. start flushes the
// queue and launches the reader. close is idempotent and joins the reader.
type trickleManager struct {
	pc     *webrtc.PeerConnection
	sig    wire.MsgConn
	key    []byte
	logger *slog.Logger

	// sendMu serializes outbound signaling sends so OnICECandidate firings
	// (any of which may produce a wire frame) and the application-driven
	// teardown send don't interleave bytes on a websocket connection.
	sendMu sync.Mutex

	mu       sync.Mutex
	queue    []webrtc.ICECandidateInit
	queueEOC bool
	started  bool
	closed   bool

	readerCancel context.CancelFunc
	readerWG     sync.WaitGroup

	teardownOnce sync.Once
	teardownCh   chan struct{}
}

func newTrickleManager(ctx context.Context, pc *webrtc.PeerConnection, sig wire.MsgConn, key []byte, logger *slog.Logger) *trickleManager {
	tm := &trickleManager{
		pc:         pc,
		sig:        sig,
		key:        key,
		logger:     logger,
		teardownCh: make(chan struct{}),
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		tm.handleLocal(ctx, c)
	})
	return tm
}

// handleLocal is invoked by pion for each gathered local candidate (and
// once with nil to signal end-of-gathering). Until start runs, the
// candidate is stashed in the queue; afterwards it is forwarded directly.
//
// pion's WASM build dispatches each invocation in its own goroutine
// (peerconnection_js.go's `go f(candidate)`); the native build invokes
// synchronously from the gatherer. sendMu serializes both cases.
func (tm *trickleManager) handleLocal(ctx context.Context, c *webrtc.ICECandidate) {
	tm.mu.Lock()
	if tm.closed {
		tm.mu.Unlock()
		return
	}
	if !tm.started {
		if c == nil {
			tm.queueEOC = true
		} else {
			tm.queue = append(tm.queue, c.ToJSON())
		}
		tm.mu.Unlock()
		return
	}
	tm.mu.Unlock()

	if err := tm.sendSignal(ctx, candidateMsg(c)); err != nil {
		tm.logger.DebugContext(ctx, "trickle: sending local candidate", slog.String("err", err.Error()))
	}
}

// candidateMsg builds a signalMsg for one local candidate. A nil c is
// encoded as an explicit end-of-candidates frame.
func candidateMsg(c *webrtc.ICECandidate) signalMsg {
	if c == nil {
		return signalMsg{Type: "end-of-candidates"}
	}
	return queuedCandidateMsg(c.ToJSON())
}

func queuedCandidateMsg(init webrtc.ICECandidateInit) signalMsg {
	return signalMsg{
		Type:             "ice-candidate",
		Candidate:        init.Candidate,
		SDPMid:           init.SDPMid,
		SDPMLineIndex:    init.SDPMLineIndex,
		UsernameFragment: init.UsernameFragment,
	}
}

// sendSignal writes one signalMsg under sendMu so concurrent trickle and
// teardown sends each produce one whole frame.
func (tm *trickleManager) sendSignal(ctx context.Context, msg signalMsg) error {
	tm.sendMu.Lock()
	defer tm.sendMu.Unlock()
	return sendSignal(ctx, tm.sig, tm.key, msg)
}

// start launches the reader and flushes any queued local candidates.
// Must be called exactly once after exchangeSDP returns. Errors flushing
// individual candidates are logged but otherwise tolerated — a candidate
// the peer never receives is just one less pair to consider, not a fatal
// condition.
//
// The reader spawns before the flush. With both peers calling start in
// the same window — each holding ~N gathered candidates and a bounded
// outbound buffer — flushing-before-spawning deadlocks: both ends
// saturate their outbound queue and neither has a reader running to
// drain the peer's bytes. Spawning first means each peer's reader is up
// to drain the other's flush.
func (tm *trickleManager) start(ctx context.Context) {
	tm.mu.Lock()
	if tm.started {
		tm.mu.Unlock()
		return
	}
	queue := tm.queue
	queueEOC := tm.queueEOC
	tm.queue = nil
	tm.started = true
	rctx, cancel := context.WithCancel(context.Background())
	tm.readerCancel = cancel
	tm.mu.Unlock()

	tm.readerWG.Go(func() { tm.runReader(rctx) })

	for _, init := range queue {
		if err := tm.sendSignal(ctx, queuedCandidateMsg(init)); err != nil {
			tm.logger.DebugContext(ctx, "trickle: flushing queued candidate", slog.String("err", err.Error()))
		}
	}
	if queueEOC {
		if err := tm.sendSignal(ctx, signalMsg{Type: "end-of-candidates"}); err != nil {
			tm.logger.DebugContext(ctx, "trickle: flushing end-of-candidates", slog.String("err", err.Error()))
		}
	}
}

// runReader is the only goroutine permitted to call sig.Recv on this
// trickleManager. It dispatches candidates to pion's agent, forwards
// end-of-candidates so the agent finalizes the remote set, and surfaces
// the peer's teardown signal via teardownCh.
func (tm *trickleManager) runReader(ctx context.Context) {
	for {
		msg, err := recvSignal(ctx, tm.sig, tm.key)
		if err != nil {
			if !isAnyOf(err, context.Canceled, context.DeadlineExceeded) {
				tm.logger.DebugContext(ctx, "trickle: reader exit", slog.String("err", err.Error()))
			}
			return
		}
		switch msg.Type {
		case "ice-candidate":
			init := webrtc.ICECandidateInit{
				Candidate:        msg.Candidate,
				SDPMid:           msg.SDPMid,
				SDPMLineIndex:    msg.SDPMLineIndex,
				UsernameFragment: msg.UsernameFragment,
			}
			if err := tm.pc.AddICECandidate(init); err != nil {
				tm.logger.DebugContext(ctx, "trickle: AddICECandidate", slog.String("err", err.Error()))
			}
		case "end-of-candidates":
			// Empty Candidate string is the WebRTC spec's signal that the
			// remote will not produce more candidates. In pion v4 this
			// reaches agent.AddRemoteCandidate(nil), which is a no-op (no
			// state transition). We forward it for spec conformance and so
			// the next pion release that does act on it observes the right
			// signal without us having to revisit this code.
			if err := tm.pc.AddICECandidate(webrtc.ICECandidateInit{}); err != nil {
				tm.logger.DebugContext(ctx, "trickle: end-of-candidates AddICECandidate", slog.String("err", err.Error()))
			}
		case teardownSignal:
			tm.teardownOnce.Do(func() { close(tm.teardownCh) })
			return
		default:
			tm.logger.DebugContext(ctx, "trickle: ignoring unknown signal type", slog.String("type", msg.Type))
		}
	}
}

// exchangeTeardown sends teardownSignal and waits for the reader to
// observe the peer's matching signal. ctx bounds the wait.
func (tm *trickleManager) exchangeTeardown(ctx context.Context) error {
	if err := tm.sendSignal(ctx, signalMsg{Type: teardownSignal}); err != nil {
		return fmt.Errorf("exchangeTeardown: sending: %w", err)
	}
	select {
	case <-tm.teardownCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("exchangeTeardown: waiting for peer: %w", ctx.Err())
	}
}

// close marks the manager closed (so handleLocal stops forwarding
// late-arriving candidates), cancels the reader, and joins it. Safe and
// idempotent across the multiple paths that may invoke it (success-path
// defer, error-path defer, Session.Close).
func (tm *trickleManager) close() {
	tm.mu.Lock()
	if tm.closed {
		tm.mu.Unlock()
		return
	}
	tm.closed = true
	cancel := tm.readerCancel
	tm.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	tm.readerWG.Wait()
}
