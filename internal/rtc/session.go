package rtc

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/pion/webrtc/v4"
)

// A Session is a persistent, bidirectional WebRTC connection between two
// peers. Either peer may stream outbound transfers via Push and accept
// inbound transfers via Pull; the two directions multiplex on the single
// underlying data channel via the framing.go tag system. Within a
// direction, transfers serialize: the next Push must wait for the previous
// Push to return. Opposite-direction transfers may run concurrently — the
// shared frame writer is mutex-protected so each Write produces one
// complete frame.
//
// Open a session by paired calls to Initiate (one peer) and Respond (the
// other). After open, the peers' APIs are symmetric — there is no
// sender/receiver role at the session level. Call Close to tear down.
//
// Wire-compatible with v1 rtc.Send / rtc.Recv: the single DC is created
// the same way and the framing tags are unchanged. A v1 sender → v2
// receiver pairing works as a one-shot transfer; the v2 receiver's Pull
// returns when the v1 sender's tagEOF arrives, and the rtc-level close
// cascades back through the trickleManager's teardown handshake.
type Session struct {
	cfg      Config
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	raw      io.ReadWriteCloser
	closeRaw func()
	w        io.Writer
	tm       *trickleManager
	route    Route

	// writeMu serializes frame writes on the data channel. Push (tagData /
	// tagEOF) and Pull's ack writer (tagAck) share the channel; each Write
	// is one complete frame, and the mutex prevents two writers from
	// interleaving frames.
	writeMu sync.Mutex

	// inFrames carries tagData / tagEOF dataFrames produced by the demuxer
	// goroutine for Pull to consume. Capacity 0 — implicit backpressure.
	inFrames chan dataFrame

	// demuxDone is closed when the demuxer exits; demuxErr captures the
	// terminal error.
	demuxDone   chan struct{}
	demuxErr    error
	demuxCancel context.CancelFunc
	demuxWG     sync.WaitGroup

	// lastAck records the cumulative byte count from the most recent tagAck
	// the demuxer observed. ackBroadcastCh is closed-and-replaced on every
	// ack, giving Push.waitAck a wakeup signal that composes with select.
	lastAck         atomic.Int64
	ackBroadcastMu  sync.Mutex
	ackBroadcastCh  chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// dataFrame is the payload of one tagData or tagEOF frame, queued by the
// demuxer for Pull to consume.
type dataFrame struct {
	eof     bool
	payload []byte
}

// Route describes the transport selected by ICE for this session: direct
// peer-to-peer (host/srflx/prflx) or relayed through a TURN server. Sampled
// once just after the data channel opens; not updated on later ICE restarts.
type Route int

const (
	RouteUnknown Route = iota
	RouteDirect
	RouteRelayed
)

func (r Route) String() string {
	switch r {
	case RouteDirect:
		return "direct"
	case RouteRelayed:
		return "relayed"
	default:
		return "unknown"
	}
}

// Initiate opens a session as the WebRTC offerer. The peer must call
// Respond. Wire-compatible with v1 rtc.Send: a v1 sender's Recv-style
// counterpart can use Respond unchanged.
func Initiate(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config) (*Session, error) {
	return openSession(ctx, sig, key, cfg, true)
}

// Respond opens a session as the WebRTC answerer. The peer must call
// Initiate. Wire-compatible with v1 rtc.Recv.
func Respond(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config) (*Session, error) {
	return openSession(ctx, sig, key, cfg, false)
}

func openSession(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config, initiator bool) (sess *Session, err error) {
	pc, err := newPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}
	defer func() {
		if err != nil {
			_ = pc.Close()
		}
	}()

	openWait := newDatachanWait()
	var rawHolder atomicRWC
	wireDC := func(d *webrtc.DataChannel) {
		d.OnOpen(func() {
			cfg.Logger.DebugContext(ctx, "dc onopen", slog.String("label", d.Label()))
			raw, err := d.Detach()
			if err != nil {
				openWait.setErr(fmt.Errorf("detaching dc: %w", err))
				return
			}
			rawHolder.set(raw)
			openWait.setRWC(raw)
		})
		// pion's wasm/JS detached data channel does not unblock its
		// blocked Read when the underlying RTCDataChannel fires onclose
		// (it only wires OnMessage). Without this hook the demuxer hangs
		// forever after the peer closes; closing the detached handle on
		// our side closes the channel that drives Read's select. The
		// native build's stream.Close already gets unblocked by the
		// peer's reflexive reset, so this is a no-op there.
		d.OnClose(func() {
			cfg.Logger.DebugContext(ctx, "dc onclose", slog.String("label", d.Label()))
			rawHolder.close()
		})
		d.OnError(func(err error) {
			cfg.Logger.DebugContext(ctx, "dc onerror", slog.String("err", err.Error()))
			rawHolder.close()
		})
	}
	var dc *webrtc.DataChannel
	if initiator {
		dc, err = pc.CreateDataChannel("conduit", nil)
		if err != nil {
			return nil, fmt.Errorf("opening session: creating dc: %w", err)
		}
		wireDC(dc)
	} else {
		pc.OnDataChannel(func(remoteDC *webrtc.DataChannel) {
			dc = remoteDC
			wireDC(remoteDC)
		})
	}
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cfg.Logger.DebugContext(ctx, "peer connection state", slog.String("state", s.String()))
		switch s {
		case webrtc.PeerConnectionStateFailed:
			openWait.setErr(fmt.Errorf("peer connection failed"))
			rawHolder.close()
		case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			// Force the local detached DC handle closed so the demuxer
			// exits even if the data-channel layer's onclose event hasn't
			// fired (or fires too late).
			rawHolder.close()
		}
	})

	// Register OnICECandidate before exchangeSDP — pion can fire
	// candidates from inside SetLocalDescription, before that call returns.
	tm := newTrickleManager(ctx, pc, sig, key, cfg.Logger)
	defer func() {
		if err != nil {
			tm.close()
		}
	}()

	if err := exchangeSDP(ctx, pc, sig, key, initiator); err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}
	// Both sides have remote descriptions set; flush queued local
	// candidates and start consuming peer candidates from sig.
	tm.start(ctx)

	raw, err := openWait.wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening session: waiting for dc open: %w", err)
	}

	demuxCtx, demuxCancel := context.WithCancel(context.Background())
	s := &Session{
		cfg:            cfg,
		pc:             pc,
		dc:             dc,
		raw:            raw,
		closeRaw:       rawHolder.close,
		w:              wrapSendWriter(dc, raw),
		tm:             tm,
		route:          detectRoute(pc),
		inFrames:       make(chan dataFrame),
		demuxDone:      make(chan struct{}),
		demuxCancel:    demuxCancel,
		ackBroadcastCh: make(chan struct{}),
	}
	s.demuxWG.Go(func() {
		s.runDemux(demuxCtx)
	})
	return s, nil
}

// exchangeSDP runs the offer/answer dance over sig. The encryption and
// signaling-message format match the v1 Send/Recv path.
func exchangeSDP(ctx context.Context, pc *webrtc.PeerConnection, sig wire.MsgConn, key []byte, initiator bool) error {
	if initiator {
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			return fmt.Errorf("creating offer: %w", err)
		}
		if err := pc.SetLocalDescription(offer); err != nil {
			return fmt.Errorf("setting local offer: %w", err)
		}
		if err := sendSignal(ctx, sig, key, signalMsg{Type: "offer", SDP: pc.LocalDescription().SDP}); err != nil {
			return fmt.Errorf("sending offer: %w", err)
		}
		answer, err := recvSignal(ctx, sig, key)
		if err != nil {
			return fmt.Errorf("receiving answer: %w", err)
		}
		if answer.Type != "answer" {
			return fmt.Errorf("expected answer, got %q", answer.Type)
		}
		if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
			return fmt.Errorf("setting remote answer: %w", err)
		}
		return nil
	}
	offer, err := recvSignal(ctx, sig, key)
	if err != nil {
		return fmt.Errorf("receiving offer: %w", err)
	}
	if offer.Type != "offer" {
		return fmt.Errorf("expected offer, got %q", offer.Type)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer.SDP}); err != nil {
		return fmt.Errorf("setting remote offer: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("creating answer: %w", err)
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("setting local answer: %w", err)
	}
	if err := sendSignal(ctx, sig, key, signalMsg{Type: "answer", SDP: pc.LocalDescription().SDP}); err != nil {
		return fmt.Errorf("sending answer: %w", err)
	}
	return nil
}

// runDemux reads frames from the data channel, routing tagData / tagEOF to
// inFrames (consumed by Pull) and tagAck to cfg.OnRemoteProgress (fired
// inline; callbacks must not block).
//
// Exits when the data channel returns any error. ctx is unused by the read
// loop — pion's stream.Close only resets the local OUTGOING half, so the
// pending Read can only be unblocked by the peer's reset (their close) or
// by pc.Close. Session.Close coordinates the peer-driven path via
// exchangeTeardown.
func (s *Session) runDemux(ctx context.Context) {
	defer close(s.demuxDone)
	defer close(s.inFrames)
	_ = ctx

	var (
		framesRead int
		bytesRead  int64
	)
	defer func() {
		s.cfg.Logger.Debug("demux exit",
			slog.Int("framesRead", framesRead),
			slog.Int64("bytesRead", bytesRead),
			slog.Any("err", s.demuxErr),
		)
	}()

	msg := make([]byte, maxFrameSize)
	for {
		n, err := s.raw.Read(msg)
		if err != nil {
			s.demuxErr = err
			return
		}
		if n == 0 {
			s.demuxErr = fmt.Errorf("demux: empty frame")
			return
		}
		framesRead++
		bytesRead += int64(n)
		switch msg[0] {
		case tagData:
			if n == 1 {
				s.demuxErr = fmt.Errorf("demux: empty data frame")
				return
			}
			s.cfg.Logger.Debug("dc rx tagData",
				slog.Int("payload", n-1),
				slog.Int("frame", framesRead),
				slog.Int64("totalBytes", bytesRead),
			)
			payload := append([]byte(nil), msg[1:n]...)
			s.inFrames <- dataFrame{payload: payload}
		case tagEOF:
			s.cfg.Logger.Debug("dc rx tagEOF",
				slog.Int("frame", framesRead),
				slog.Int64("totalBytes", bytesRead),
			)
			s.inFrames <- dataFrame{eof: true}
		case tagAck:
			if n != ackFrameSize {
				s.demuxErr = fmt.Errorf("demux: ack frame size = %d, want %d", n, ackFrameSize)
				return
			}
			total := int64(binary.BigEndian.Uint64(msg[1:n]))
			s.cfg.Logger.Debug("dc rx tagAck",
				slog.Int64("total", total),
				slog.Int("frame", framesRead),
			)
			s.recordAck(total)
			if s.cfg.OnRemoteProgress != nil {
				s.cfg.OnRemoteProgress(total)
			}
		default:
			s.cfg.Logger.Debug("dc rx unknown tag",
				slog.Int("tag", int(msg[0])),
				slog.Int("size", n),
			)
		}
	}
}

// Push writes one outbound transfer: src is copied to the peer prefixed
// with tagData frames and terminated by tagEOF, then waits for the peer's
// final ack to cover every byte written. The ack barrier matters: without
// it the caller's deferred Close races into the rtc teardown while the
// peer is still draining buffered frames, and the peer can miss tagEOF
// (the receiver's read loop sees the data channel reset before consuming
// the final frame). Not safe to call concurrently with itself.
//
// lastAck is reset to zero at the start of each Push because the receiver's
// ackingWriter rebuilds per Pull — every transfer's ack count starts at 0
// and grows to that transfer's byte total. Each Push waits for the
// transfer's own n, not a session-cumulative total.
func (s *Session) Push(ctx context.Context, src io.Reader) error {
	s.lastAck.Store(0)
	s.cfg.Logger.Debug("push start")
	tw := &lockedTagWriter{w: s.w, mu: &s.writeMu, logger: s.cfg.Logger}
	n, err := io.Copy(tw, src)
	if err != nil {
		return fmt.Errorf("push: copying payload: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("push: writing eof: %w", err)
	}
	s.cfg.Logger.Debug("push waiting for ack",
		slog.Int64("target", n),
		slog.Int("framesSent", tw.frames),
	)
	if err := s.waitAck(ctx, n); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	s.cfg.Logger.Debug("push complete", slog.Int64("acked", n))
	return nil
}

// recordAck stores the cumulative ack total and wakes any waiter blocked
// in waitAck. The closed-and-replaced channel pattern lets multiple
// waiters notice the wakeup without losing edges between writes.
func (s *Session) recordAck(total int64) {
	prev := s.lastAck.Swap(total)
	s.ackBroadcastMu.Lock()
	close(s.ackBroadcastCh)
	s.ackBroadcastCh = make(chan struct{})
	s.ackBroadcastMu.Unlock()
	s.cfg.Logger.Debug("ack received",
		slog.Int64("total", total),
		slog.Int64("prev", prev),
	)
}

// ackChannel returns the current broadcast channel under the lock that
// guards close-and-replace. Callers must take the channel BEFORE checking
// lastAck to avoid missing a signal that lands between the check and the
// select.
func (s *Session) ackChannel() <-chan struct{} {
	s.ackBroadcastMu.Lock()
	defer s.ackBroadcastMu.Unlock()
	return s.ackBroadcastCh
}

// waitAckStallTimeout bounds how long waitAck will sit on a non-advancing
// lastAck before giving up. Real transfers either complete (tagEOF flush
// covers `n` instantly) or make incremental progress through the
// receiver's keepalive (every 2s once the receiver has consumed any
// bytes). If neither happens for this long, the data path is wedged
// somewhere below the application layer — receiver's pc.Close cascade
// eventually wakes us via demuxDone, but we shouldn't depend on
// kernel-level UDP timeouts to surface the failure to the user. Pick
// long enough that a slow but healthy transfer never trips it: 30s
// without any ack progress is well past anything pion's congestion
// control would take to recover from.
const waitAckStallTimeout = 30 * time.Second

// waitAck blocks until lastAck reaches target, the demuxer exits, or ctx
// is cancelled. The receiver's flush() at tagEOF emits a final ack
// covering every byte written, so for a successful transfer this returns
// promptly after the peer drains the stream. If lastAck does not advance
// for waitAckStallTimeout we surface a stall error rather than hang.
func (s *Session) waitAck(ctx context.Context, target int64) error {
	lastObserved := s.lastAck.Load()
	deadline := time.Now().Add(waitAckStallTimeout)
	for {
		ch := s.ackChannel()
		current := s.lastAck.Load()
		if current >= target {
			return nil
		}
		if current != lastObserved {
			lastObserved = current
			deadline = time.Now().Add(waitAckStallTimeout)
		}
		stallTimer := time.NewTimer(time.Until(deadline))
		select {
		case <-ch:
			// recordAck fired; loop and re-check lastAck.
		case <-s.demuxDone:
			stallTimer.Stop()
			if s.lastAck.Load() >= target {
				return nil
			}
			return fmt.Errorf("session ended before final ack (acked %d, want %d)", s.lastAck.Load(), target)
		case <-ctx.Done():
			stallTimer.Stop()
			return ctx.Err()
		case <-stallTimer.C:
			return fmt.Errorf("transfer stalled: ack did not advance for %v (acked %d, want %d)", waitAckStallTimeout, current, target)
		}
		stallTimer.Stop()
	}
}

// pullKeepaliveInterval bounds how long the sender's waitAck waits when
// the body bytes are all delivered but tagEOF hasn't surfaced yet (an
// SCTP-layer hiccup we've observed on real-world WAN transfers — the
// large body chunk arrives and is SACKed but the trailing 1-byte tagEOF
// chunk never makes it up to the receiver's stream queue). On every
// tick, Pull flushes an ack frame with the current cumulative byte
// count; once the receiver has consumed everything the sender wrote,
// that flush carries `n` and the sender's waitAck unblocks. The sender
// then closes, which sends an SCTP stream reset that the receiver
// surfaces as EOF on the pipe — age decrypts the partial last chunk
// via ErrUnexpectedEOF and handleInbound completes cleanly.
//
// Picked larger than typical end-to-end latency so healthy transfers
// still complete on tagEOF (the ticker simply never fires) and slow
// transfers see at most a handful of extra 9-byte ack frames.
const pullKeepaliveInterval = 2 * time.Second

// Pull reads one inbound transfer to dst, writing tagAck progress frames
// back to the peer as bytes accumulate. Returns when the demuxer surfaces
// the transfer's tagEOF. Not safe to call concurrently with itself.
//
// Returns io.EOF if the session ended before a transfer started; any other
// error reflects a protocol violation, a write failure on dst, or a
// session-level cancellation.
func (s *Session) Pull(ctx context.Context, dst io.Writer) error {
	aw := &sessionAckingWriter{
		dst:          dst,
		raw:          s.w,
		mu:           &s.writeMu,
		logger:       s.cfg.Logger,
		ackThreshold: defaultAckThreshold,
	}
	s.cfg.Logger.Debug("pull start")
	keepalive := time.NewTicker(pullKeepaliveInterval)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			s.cfg.Logger.Debug("pull return ctx",
				slog.Int64("totalConsumed", aw.written),
				slog.Int("acksEmitted", aw.acks),
			)
			return fmt.Errorf("pull: %w", ctx.Err())
		case <-keepalive.C:
			// Re-emit the running cumulative ack so the sender's
			// waitAck can make progress even if a trailing tagEOF
			// got stuck below the application layer. Idempotent —
			// repeated acks at the same total are harmless on the
			// peer.
			if err := aw.flush("keepalive"); err != nil {
				return fmt.Errorf("pull: flushing keepalive ack: %w", err)
			}
		case frame, ok := <-s.inFrames:
			if !ok {
				if s.demuxErr != nil {
					s.cfg.Logger.Debug("pull return demux exit",
						slog.String("err", s.demuxErr.Error()),
						slog.Int64("totalConsumed", aw.written),
						slog.Int("acksEmitted", aw.acks),
					)
					return fmt.Errorf("pull: demuxer exited: %w", s.demuxErr)
				}
				s.cfg.Logger.Debug("pull return inFrames closed",
					slog.Int64("totalConsumed", aw.written),
					slog.Int("acksEmitted", aw.acks),
				)
				return io.EOF
			}
			if frame.eof {
				if err := aw.flush("tagEOF"); err != nil {
					return fmt.Errorf("pull: flushing final ack: %w", err)
				}
				s.cfg.Logger.Debug("pull return tagEOF",
					slog.Int64("totalConsumed", aw.written),
					slog.Int("acksEmitted", aw.acks),
				)
				return nil
			}
			if _, err := aw.Write(frame.payload); err != nil {
				return fmt.Errorf("pull: writing payload: %w", err)
			}
		}
	}
}

// Close tears down the session. The teardown handshake is best-effort
// with a short timeout so a peer that wants to keep its half of the
// session open (e.g. a browser sender that has more transfers to do) does
// not pin this peer's Close indefinitely. Whether or not teardown
// completes, the data channel is closed locally; the peer's WebRTC stack
// observes the cascade and surfaces a connection-closed error to its own
// session machinery.
//
// Idempotent — second and later calls return the first error.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		teardownCtx, teardownCancel := context.WithTimeout(ctx, teardownTimeout)
		err := s.tm.exchangeTeardown(teardownCtx)
		teardownCancel()
		if err != nil {
			s.cfg.Logger.DebugContext(ctx, "session teardown handshake", slog.String("err", err.Error()))
			// Don't propagate as a Close error — the local close still
			// succeeds and the peer-disagreement case is expected when
			// the other side wants its session to remain open.
		}
		s.closeRaw()
		closeTimer := time.NewTimer(5 * time.Second)
		defer closeTimer.Stop()
		select {
		case <-s.demuxDone:
		case <-ctx.Done():
			s.cfg.Logger.DebugContext(ctx, "session close: ctx done before demuxer exit")
		case <-closeTimer.C:
			s.cfg.Logger.DebugContext(ctx, "session close: demuxer didn't exit within 5s")
		}
		s.demuxCancel()
		// Close trickle before pc so handleLocal stops sending before
		// pion's gatherer fires its final OnICECandidate(nil) at shutdown.
		s.tm.close()
		// pc.Close blocks inside pion's sctp.Abort (two ~200ms write/read
		// deadlines) when the peer's transport is already torn down — the
		// common case on the receiver, since the sender's pc.Close runs first
		// and tears down DTLS/UDP from under us. On real networks those
		// deadlines compound and can hang for multiple seconds. Run pc.Close
		// in a background goroutine and bound the user-visible wait; on
		// timeout the goroutine continues to drain pion's internals while
		// our caller proceeds. The process or surrounding loop will outlive
		// it (one-shot CLI exits, watch loops open a fresh pc per pairing).
		pcCloseDone := make(chan struct{})
		go func() {
			_ = s.pc.Close()
			close(pcCloseDone)
		}()
		pcTimer := time.NewTimer(pcCloseBudget)
		defer pcTimer.Stop()
		select {
		case <-pcCloseDone:
		case <-pcTimer.C:
			s.cfg.Logger.DebugContext(ctx, "session close: pc.Close exceeded budget, draining in background")
		}
		s.demuxWG.Wait()
	})
	return s.closeErr
}

// Route reports the transport ICE selected for this session — direct
// peer-to-peer or relayed through TURN. Sampled at session open.
func (s *Session) Route() Route {
	return s.route
}

// teardownTimeout bounds exchangeTeardown during Close. Cooperating peers
// respond in low double-digit milliseconds, so 500ms is long enough for
// the common case yet short enough that a session-mode peer (which
// deliberately stays open between transfers) doesn't add a perceptible
// pause to the local Close. After the timeout the SCTP reset from
// raw.Close still propagates and the peer's data channel surfaces a
// close event within a few additional milliseconds.
const teardownTimeout = 500 * time.Millisecond

// pcCloseBudget bounds the user-visible wait for pion's pc.Close. The
// sender's close runs first and tears down DTLS/UDP, so by the time the
// receiver's pc.Close runs the underlying transport is already gone and
// pion's sctp.Abort stacks two ~200ms deadlines waiting for an ack that
// can't arrive. 100ms covers the common cases where pion's internals
// finish synchronously; beyond that we let the goroutine drain in the
// background rather than make the user wait on transport cleanup.
const pcCloseBudget = 100 * time.Millisecond

// atomicRWC is a one-set io.ReadWriteCloser slot, safe to set from OnOpen
// and read from OnClose-style callbacks without ordering assumptions about
// which fires first. close() is idempotent — all of the data-channel
// teardown paths (DC onclose, DC onerror, PC state change, Session.Close)
// route through the same sync.OnceValue-wrapped underlying Close, so
// pion-wasm's non-idempotent detachedDataChannel.Close (which panics on
// `close(c.done)` if called twice) only sees one invocation.
type atomicRWC struct {
	mu      sync.Mutex
	closeFn func() error
}

func (a *atomicRWC) set(r io.ReadWriteCloser) {
	a.mu.Lock()
	a.closeFn = sync.OnceValue(r.Close)
	a.mu.Unlock()
}

func (a *atomicRWC) close() {
	a.mu.Lock()
	fn := a.closeFn
	a.mu.Unlock()
	if fn != nil {
		_ = fn()
	}
}

// lockedTagWriter is tagWriter with a mutex around each frame Write so it
// shares the data channel safely with sessionAckingWriter, and chunks
// large inputs so each underlying Write produces one DC message no larger
// than maxPayloadPerFrame. v1's tagWriter never had to chunk because
// wire.Encrypt upstream already produced bounded chunks; rtc.Session is
// plaintext at the transport layer, so a caller passing a multi-MB slice
// (e.g. via bytes.Reader.WriteTo) would otherwise produce a single
// oversize SCTP message that blows past the receiver's read buffer.
type lockedTagWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	logger *slog.Logger
	closed bool
	frames int
	bytes  int64
}

// maxPayloadPerFrame caps each tagData frame so the resulting SCTP
// user message fits in a single MTU-sized DATA chunk and pion never
// needs to fragment. Pion's outboundMTU is 1200 and per-chunk
// overhead (SCTP common header + DATA chunk header) is 28 bytes,
// leaving 1172 bytes of useful payload per chunk; a 1-byte tag plus
// 1024 bytes of payload (1025 total) sits comfortably under that.
//
// We chose this aggressive cap after observing real-world WAN
// stalls where the first multi-fragment message after a stream of
// single-chunk messages caused the receiver's DTLS read to time
// out and tear down the association. Whether the root cause is a
// NAT that drops bursts of UDP packets or a pion SCTP bug
// triggered by post-fragmentation reassembly, single-chunk frames
// sidestep it. The cost is one extra tag byte and a tiny amount
// of pump-loop overhead per kilobyte — under 1% on a 37 KiB
// payload.
const maxPayloadPerFrame = 1024

func (t *lockedTagWriter) Write(p []byte) (int, error) {
	if t.closed {
		return 0, fmt.Errorf("write after close")
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxPayloadPerFrame {
			chunk = chunk[:maxPayloadPerFrame]
		}
		buf := make([]byte, 1+len(chunk))
		buf[0] = tagData
		copy(buf[1:], chunk)
		t.mu.Lock()
		_, err := t.w.Write(buf)
		t.mu.Unlock()
		if err != nil {
			return written, fmt.Errorf("writing data frame: %w", err)
		}
		t.frames++
		t.bytes += int64(len(chunk))
		t.logger.Debug("dc tx tagData",
			slog.Int("payload", len(chunk)),
			slog.Int("frame", t.frames),
			slog.Int64("totalBytes", t.bytes),
		)
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

func (t *lockedTagWriter) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.w.Write([]byte{tagEOF}); err != nil {
		return fmt.Errorf("writing eof frame: %w", err)
	}
	t.logger.Debug("dc tx tagEOF",
		slog.Int("framesSent", t.frames),
		slog.Int64("totalBytes", t.bytes),
	)
	return nil
}

// sessionAckingWriter is ackingWriter with a mutex around each ack frame
// Write so it shares the data channel safely with lockedTagWriter.
type sessionAckingWriter struct {
	dst          io.Writer
	raw          io.Writer
	mu           *sync.Mutex
	logger       *slog.Logger
	written      int64
	sinceLastAck int64
	ackThreshold int64
	acks         int
}

func (a *sessionAckingWriter) Write(p []byte) (int, error) {
	n, err := a.dst.Write(p)
	if n > 0 {
		a.written += int64(n)
		a.sinceLastAck += int64(n)
		a.logger.Debug("pull consumed payload",
			slog.Int("bytes", n),
			slog.Int64("totalConsumed", a.written),
		)
		if a.sinceLastAck >= a.ackThreshold {
			if aerr := a.writeAckLocked(a.written, "threshold"); aerr != nil {
				return n, fmt.Errorf("emitting ack: %w", aerr)
			}
			a.sinceLastAck = 0
		}
	}
	return n, err
}

func (a *sessionAckingWriter) flush(reason string) error {
	if a.written == 0 {
		return nil
	}
	if err := a.writeAckLocked(a.written, reason); err != nil {
		return fmt.Errorf("flushing final ack: %w", err)
	}
	a.sinceLastAck = 0
	return nil
}

func (a *sessionAckingWriter) writeAckLocked(total int64, reason string) error {
	var buf [ackFrameSize]byte
	buf[0] = tagAck
	binary.BigEndian.PutUint64(buf[1:], uint64(total))
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.raw.Write(buf[:]); err != nil {
		return fmt.Errorf("writing ack frame: %w", err)
	}
	a.acks++
	a.logger.Debug("dc tx tagAck",
		slog.String("reason", reason),
		slog.Int64("total", total),
		slog.Int("ackNum", a.acks),
	)
	return nil
}
