package rtc

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"sync"
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
// cascades back through exchangeTeardown.
type Session struct {
	cfg Config
	pc  *webrtc.PeerConnection
	dc  *webrtc.DataChannel
	raw io.ReadWriteCloser
	w   io.Writer
	sig wire.MsgConn
	key []byte

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

	closeOnce sync.Once
	closeErr  error
}

// dataFrame is the payload of one tagData or tagEOF frame, queued by the
// demuxer for Pull to consume.
type dataFrame struct {
	eof     bool
	payload []byte
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
			if raw, ok := rawHolder.get(); ok {
				_ = raw.Close()
			}
		})
		d.OnError(func(err error) {
			cfg.Logger.DebugContext(ctx, "dc onerror", slog.String("err", err.Error()))
			if raw, ok := rawHolder.get(); ok {
				_ = raw.Close()
			}
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
			if raw, ok := rawHolder.get(); ok {
				_ = raw.Close()
			}
		case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			// Force the local detached DC handle closed so the demuxer
			// exits even if the data-channel layer's onclose event hasn't
			// fired (or fires too late). The native build's own SCTP
			// close path is unaffected because the handle is already
			// closed by the time we get here in that scenario.
			if raw, ok := rawHolder.get(); ok {
				_ = raw.Close()
			}
		}
	})

	if err := exchangeSDP(ctx, pc, sig, key, initiator); err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	raw, err := openWait.wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening session: waiting for dc open: %w", err)
	}

	demuxCtx, demuxCancel := context.WithCancel(context.Background())
	s := &Session{
		cfg:         cfg,
		pc:          pc,
		dc:          dc,
		raw:         raw,
		w:           wrapSendWriter(dc, raw),
		sig:         sig,
		key:         key,
		inFrames:    make(chan dataFrame),
		demuxDone:   make(chan struct{}),
		demuxCancel: demuxCancel,
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
		if err := waitGather(ctx, pc); err != nil {
			return fmt.Errorf("ice gather: %w", err)
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
	if err := waitGather(ctx, pc); err != nil {
		return fmt.Errorf("ice gather: %w", err)
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
		switch msg[0] {
		case tagData:
			if n == 1 {
				s.demuxErr = fmt.Errorf("demux: empty data frame")
				return
			}
			payload := append([]byte(nil), msg[1:n]...)
			s.inFrames <- dataFrame{payload: payload}
		case tagEOF:
			s.inFrames <- dataFrame{eof: true}
		case tagAck:
			if n != ackFrameSize {
				s.demuxErr = fmt.Errorf("demux: ack frame size = %d, want %d", n, ackFrameSize)
				return
			}
			total := int64(binary.BigEndian.Uint64(msg[1:n]))
			if s.cfg.OnRemoteProgress != nil {
				s.cfg.OnRemoteProgress(total)
			}
		default:
			// Unknown tag: ignore for forward compatibility, matches readAcks.
		}
	}
}

// Push writes one outbound transfer: src is copied to the peer prefixed
// with tagData frames and terminated by tagEOF. Returns once the framing
// terminator has been written. Acks for this transfer flow asynchronously
// through cfg.OnRemoteProgress and may continue to arrive after Push
// returns. Not safe to call concurrently with itself.
func (s *Session) Push(ctx context.Context, src io.Reader) error {
	tw := &lockedTagWriter{w: s.w, mu: &s.writeMu}
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("push: copying payload: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("push: writing eof: %w", err)
	}
	_ = ctx
	return nil
}

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
		ackThreshold: defaultAckThreshold,
	}
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("pull: %w", ctx.Err())
		case frame, ok := <-s.inFrames:
			if !ok {
				if s.demuxErr != nil {
					return fmt.Errorf("pull: demuxer exited: %w", s.demuxErr)
				}
				return io.EOF
			}
			if frame.eof {
				if err := aw.flush(); err != nil {
					return fmt.Errorf("pull: flushing final ack: %w", err)
				}
				return nil
			}
			if _, err := aw.Write(frame.payload); err != nil {
				return fmt.Errorf("pull: writing payload: %w", err)
			}
		}
	}
}

// Close tears down the session. exchangeTeardown is best-effort with a
// short timeout so a peer that wants to keep its half of the session open
// (e.g. a browser sender that has more transfers to do) does not pin this
// peer's Close indefinitely. Whether or not teardown completes, the data
// channel is closed locally; the peer's WebRTC stack observes the cascade
// and surfaces a connection-closed error to its own session machinery.
//
// Idempotent — second and later calls return the first error.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		teardownCtx, teardownCancel := context.WithTimeout(ctx, teardownTimeout)
		err := exchangeTeardown(teardownCtx, s.sig, s.key)
		teardownCancel()
		if err != nil {
			s.cfg.Logger.DebugContext(ctx, "session teardown handshake", slog.String("err", err.Error()))
			// Don't propagate as a Close error — the local close still
			// succeeds and the peer-disagreement case is expected when
			// the other side wants its session to remain open.
		}
		_ = s.raw.Close()
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
		if err := s.pc.Close(); err != nil && s.closeErr == nil {
			s.closeErr = fmt.Errorf("close: peer connection: %w", err)
		}
		s.demuxWG.Wait()
	})
	return s.closeErr
}

// teardownTimeout bounds exchangeTeardown during Close. Cooperating peers
// respond in low double-digit milliseconds, so 500ms is long enough for
// the common case yet short enough that a session-mode peer (which
// deliberately stays open between transfers) doesn't add a perceptible
// pause to the local Close. After the timeout the SCTP reset from
// raw.Close still propagates and the peer's data channel surfaces a
// close event within a few additional milliseconds.
const teardownTimeout = 500 * time.Millisecond

// atomicRWC is a one-set io.ReadWriteCloser slot, safe to set from
// OnOpen and read from OnClose without ordering assumptions about which
// callback fires first.
type atomicRWC struct {
	mu  sync.Mutex
	rwc io.ReadWriteCloser
}

func (a *atomicRWC) set(r io.ReadWriteCloser) {
	a.mu.Lock()
	a.rwc = r
	a.mu.Unlock()
}

func (a *atomicRWC) get() (io.ReadWriteCloser, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rwc, a.rwc != nil
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
	closed bool
}

const maxPayloadPerFrame = 64 * 1024

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
	return nil
}

// sessionAckingWriter is ackingWriter with a mutex around each ack frame
// Write so it shares the data channel safely with lockedTagWriter.
type sessionAckingWriter struct {
	dst          io.Writer
	raw          io.Writer
	mu           *sync.Mutex
	written      int64
	sinceLastAck int64
	ackThreshold int64
}

func (a *sessionAckingWriter) Write(p []byte) (int, error) {
	n, err := a.dst.Write(p)
	if n > 0 {
		a.written += int64(n)
		a.sinceLastAck += int64(n)
		if a.sinceLastAck >= a.ackThreshold {
			if aerr := a.writeAckLocked(a.written); aerr != nil {
				return n, fmt.Errorf("emitting ack: %w", aerr)
			}
			a.sinceLastAck = 0
		}
	}
	return n, err
}

func (a *sessionAckingWriter) flush() error {
	if a.written == 0 {
		return nil
	}
	if err := a.writeAckLocked(a.written); err != nil {
		return fmt.Errorf("flushing final ack: %w", err)
	}
	a.sinceLastAck = 0
	return nil
}

func (a *sessionAckingWriter) writeAckLocked(total int64) error {
	var buf [ackFrameSize]byte
	buf[0] = tagAck
	binary.BigEndian.PutUint64(buf[1:], uint64(total))
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.raw.Write(buf[:]); err != nil {
		return fmt.Errorf("writing ack frame: %w", err)
	}
	return nil
}
