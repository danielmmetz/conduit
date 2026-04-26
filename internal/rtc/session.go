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
// inbound transfers via Pull; the two directions ride on independent SCTP
// streams and run concurrently. Within a direction, transfers serialize:
// the next Push must wait for the previous Push to return.
//
// Open a session by paired calls to Initiate (one peer) and Respond (the
// other). After Open, the peers' APIs are symmetric — there is no
// sender/receiver role at the session level. Call Close to tear down.
//
// The two data channels are pre-negotiated with fixed SCTP stream IDs, so
// both peers create them locally with identical parameters. The DC at ID 0
// (i2r) is written by the initiator, read by the responder. The DC at ID 1
// (r2i) is written by the responder, read by the initiator. Each Session
// remembers which is its outbound and which is its inbound from the role
// it played at open time.
type Session struct {
	cfg    Config
	pc     *webrtc.PeerConnection
	outDC  *webrtc.DataChannel
	outRaw io.ReadWriteCloser
	outW   io.Writer
	inRaw  io.ReadWriteCloser
	sig    wire.MsgConn
	key    []byte

	// outMu serializes frame writes on the outbound DC. Both Push (payload
	// tags) and Pull (ack tags) write here. Each Write is one complete
	// frame; the mutex prevents two writers from interleaving frames.
	outMu sync.Mutex

	// inFrames carries tagData / tagEOF dataFrames produced by the demuxer
	// goroutine for Pull to consume. Capacity 0 — implicit backpressure.
	inFrames chan dataFrame

	// demuxDone is closed when the demuxer exits; demuxErr captures the
	// terminal error (io.EOF or io.ErrClosedPipe on clean session close,
	// otherwise the underlying read error).
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

const (
	dcLabelInitToResp        = "i2r"
	dcLabelRespToInit        = "r2i"
	dcStreamInitToResp uint16 = 0
	dcStreamRespToInit uint16 = 1
)

// Initiate opens a session as the WebRTC offerer. The peer must call
// Respond. Returns a ready-to-use Session or the first error encountered.
func Initiate(ctx context.Context, sig wire.MsgConn, key []byte, cfg Config) (*Session, error) {
	return openSession(ctx, sig, key, cfg, true)
}

// Respond opens a session as the WebRTC answerer. The peer must call
// Initiate.
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

	negotiated := true
	i2rID := dcStreamInitToResp
	r2iID := dcStreamRespToInit
	i2rDC, err := pc.CreateDataChannel(dcLabelInitToResp, &webrtc.DataChannelInit{Negotiated: &negotiated, ID: &i2rID})
	if err != nil {
		return nil, fmt.Errorf("opening session: creating i2r dc: %w", err)
	}
	r2iDC, err := pc.CreateDataChannel(dcLabelRespToInit, &webrtc.DataChannelInit{Negotiated: &negotiated, ID: &r2iID})
	if err != nil {
		return nil, fmt.Errorf("opening session: creating r2i dc: %w", err)
	}

	i2rWait := newDatachanWait()
	r2iWait := newDatachanWait()
	i2rDC.OnOpen(func() {
		raw, err := i2rDC.Detach()
		if err != nil {
			i2rWait.setErr(fmt.Errorf("detaching i2r dc: %w", err))
			return
		}
		i2rWait.setRWC(raw)
	})
	r2iDC.OnOpen(func() {
		raw, err := r2iDC.Detach()
		if err != nil {
			r2iWait.setErr(fmt.Errorf("detaching r2i dc: %w", err))
			return
		}
		r2iWait.setRWC(raw)
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		cfg.Logger.DebugContext(ctx, "peer connection state", slog.String("state", s.String()))
		if s == webrtc.PeerConnectionStateFailed {
			i2rWait.setErr(fmt.Errorf("peer connection failed"))
			r2iWait.setErr(fmt.Errorf("peer connection failed"))
		}
	})

	if err := exchangeSDP(ctx, pc, sig, key, initiator); err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	i2rRaw, err := i2rWait.wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening session: waiting for i2r open: %w", err)
	}
	r2iRaw, err := r2iWait.wait(ctx)
	if err != nil {
		_ = i2rRaw.Close()
		return nil, fmt.Errorf("opening session: waiting for r2i open: %w", err)
	}

	var outDC *webrtc.DataChannel
	var outRaw, inRaw io.ReadWriteCloser
	if initiator {
		outDC, outRaw, inRaw = i2rDC, i2rRaw, r2iRaw
	} else {
		outDC, outRaw, inRaw = r2iDC, r2iRaw, i2rRaw
	}

	demuxCtx, demuxCancel := context.WithCancel(context.Background())
	s := &Session{
		cfg:         cfg,
		pc:          pc,
		outDC:       outDC,
		outRaw:      outRaw,
		outW:        wrapSendWriter(outDC, outRaw),
		inRaw:       inRaw,
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
// signaling-message format match the v1 Send/Recv path so a session-mode
// peer is wire-compatible at the SDP layer.
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

// runDemux reads frames from the inbound DC, routing tagData / tagEOF to
// inFrames (consumed by Pull) and tagAck to cfg.OnRemoteProgress (fired
// inline; callbacks must not block).
//
// Exits when the inbound reader returns any error — typically io.EOF when
// the peer closes its outbound DC during Session.Close, or a transport
// error when pc.Close tears the underlying SCTP transport down. The
// terminal error is recorded on demuxErr; demuxDone is then closed.
//
// pion's stream.Close only resets the local OUTGOING half, so a local
// inRaw.Close cannot unblock a pending Read on the same handle — only the
// peer resetting its outgoing (their outRaw.Close) or pc.Close on either
// side can. ctx is therefore unused inside the read loop; we rely on
// Session.Close's coordinated teardown to make Read return.
func (s *Session) runDemux(ctx context.Context) {
	defer close(s.demuxDone)
	defer close(s.inFrames)
	_ = ctx

	msg := make([]byte, maxFrameSize)
	for {
		n, err := s.inRaw.Read(msg)
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
// returns; callers should not interpret Push's return as "peer has fully
// received" — only as "I have finished writing." Push is not safe to call
// concurrently with itself; serialize per session.
func (s *Session) Push(ctx context.Context, src io.Reader) error {
	tw := &lockedTagWriter{w: s.outW, mu: &s.outMu}
	if _, err := io.Copy(tw, src); err != nil {
		return fmt.Errorf("push: copying payload: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("push: writing eof: %w", err)
	}
	_ = ctx // currently unused; reserved for a future ack-bound completion.
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
		raw:          s.outW,
		mu:           &s.outMu,
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

// Close tears down the session. Both peers must call Close — exchangeTeardown
// is a synchronous handshake on the signaling channel, and the demuxer can
// only exit once the peer also closes its outbound DC (pion's stream.Close
// resets outgoing only; local Read can't be unblocked unilaterally). The
// sequence:
//
//  1. exchangeTeardown — coordinate that both peers are about to close.
//  2. Close outRaw — peer's inbound demuxer drains and exits.
//  3. Wait for our demuxer to exit (peer's Close has reset their outbound,
//     our inRaw read sees EOF). pc.Close is the safety net at end.
//  4. Close pc.
//
// Idempotent — second and later calls return the first error.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if err := exchangeTeardown(ctx, s.sig, s.key); err != nil {
			s.cfg.Logger.DebugContext(ctx, "session teardown handshake", slog.String("err", err.Error()))
			s.closeErr = fmt.Errorf("close: teardown: %w", err)
		}
		_ = s.outRaw.Close()
		// Wait for the demuxer to exit on peer's reciprocal close. Bound
		// the wait by ctx and by a 5s safety timer; pc.Close at the end
		// will force any laggard read to return.
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

// lockedTagWriter is tagWriter with a mutex around each frame Write so it
// shares the outbound DC safely with sessionAckingWriter, and chunks large
// inputs so each underlying Write produces one DC message no larger than
// maxPayloadPerFrame. v1's tagWriter never had to chunk because wire.Encrypt
// upstream already produced bounded chunks; rtc.Session is plaintext at the
// transport layer, so a caller passing a multi-MB slice (e.g. via
// bytes.Reader.WriteTo) would otherwise produce a single oversize SCTP
// message that blows past the receiver's read buffer.
type lockedTagWriter struct {
	w      io.Writer
	mu     *sync.Mutex
	closed bool
}

// maxPayloadPerFrame caps the application-layer bytes per tagData message.
// Combined with the one-byte tag and SCTP overhead, the on-wire frame fits
// well under both maxFrameSize (the receiver's buffer) and the 256 KiB
// browser ceiling that wrapSendWriter targets in the JS build.
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
// Write so it shares the outbound DC safely with lockedTagWriter.
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
