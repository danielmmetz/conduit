package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/coder/websocket"
	"github.com/danielmmetz/conduit/internal/rtc"
	"github.com/danielmmetz/conduit/internal/wire"
	"golang.org/x/sync/errgroup"
)

// A Session is a persistent, bidirectional conduit between two peers. Built
// on top of rtc.Session, it adds slot reservation / PAKE on open, age
// encryption per transfer, and the wire.Preamble framing the v1 path uses.
//
// Either peer may Push at any time; inbound transfers are delivered to the
// SinkOpener supplied at open time, fired once per transfer. Push and the
// inbound pump run on independent rtc-level data channels, so an outbound
// transfer does not block an inbound one.
//
// Both peers must call Close. The signaling channel is used for the v1
// teardown handshake exactly once, at session close.
type Session struct {
	rtc        *rtc.Session
	conn       *websocket.Conn
	key        []byte
	logger     *slog.Logger
	onTransfer SinkOpener

	pumpDone chan struct{}
	pumpErr  error
	pumpCtx  context.Context //nolint:containedctx // tied to session lifetime, cancelled by Close
	pumpStop context.CancelFunc
	pumpWG   sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// OpenSender reserves a slot, runs PAKE once a peer joins, then opens an
// rtc-level session as the WebRTC initiator. onCode is invoked with the
// human-readable code as soon as the slot is reserved (before the peer is
// known to be online), so callers can display it for the user.
//
// onTransfer is invoked once per inbound transfer, after that transfer's
// preamble has been read; the returned writer receives the plaintext
// payload bytes and is closed when the transfer ends. May be nil if the
// caller will not accept inbound transfers — in that case any inbound
// transfer terminates the session with an error.
func OpenSender(ctx context.Context, logger *slog.Logger, server string, policy RelayPolicy, onCode func(code string), onTransfer SinkOpener) (*Session, error) {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return nil, fmt.Errorf("resolving websocket URL: %w", err)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer func() {
		if err != nil {
			conn.CloseNow()
		}
	}()

	if err := writeCtl(ctx, conn, wire.ClientHello{Op: wire.OpReserve}); err != nil {
		return nil, fmt.Errorf("opening sender: sending reserve: %w", err)
	}
	reserved, err := readCtl(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("opening sender: reading reserved: %w", err)
	}
	if err := expectOp(reserved, wire.OpReserved); err != nil {
		return nil, fmt.Errorf("opening sender: %w", err)
	}
	logger.DebugContext(ctx, "slot reserved", slog.Uint64("slot", uint64(reserved.Slot)))

	code, err := wire.FormatCode(reserved.Slot)
	if err != nil {
		return nil, fmt.Errorf("opening sender: formatting code: %w", err)
	}
	if onCode != nil {
		onCode(code)
	}
	parsed, err := wire.ParseCode(code)
	if err != nil {
		return nil, fmt.Errorf("opening sender: parsing code: %w", err)
	}

	paired, err := readCtl(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("opening sender: reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return nil, fmt.Errorf("opening sender: %w", err)
	}

	key, err := wire.SendHandshakeMsg(ctx, wsMsgConn{conn: conn}, parsed)
	if err != nil {
		return nil, fmt.Errorf("opening sender: pake: %w", err)
	}
	logger.DebugContext(ctx, "pake key derived")

	ice := iceServersFromEnvelope(paired)
	if policy == RelayNone {
		ice = nil
	}
	cfg := rtc.Config{
		ICEServers:      ice,
		TransportPolicy: policy.transportPolicy(),
		Logger:          logger,
	}
	rs, err := rtc.Initiate(ctx, wsMsgConn{conn: conn}, key, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening sender: rtc initiate: %w", err)
	}

	return startSession(rs, conn, key, logger, onTransfer), nil
}

// OpenReceiver joins a slot, runs PAKE, and opens an rtc-level session as
// the WebRTC responder. onTransfer fires for each inbound transfer.
func OpenReceiver(ctx context.Context, logger *slog.Logger, server string, code wire.Code, policy RelayPolicy, onTransfer SinkOpener) (*Session, error) {
	wsURL, err := wsURLFor(server)
	if err != nil {
		return nil, fmt.Errorf("resolving websocket URL: %w", err)
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dialing %s: %w", wsURL, err)
	}
	defer func() {
		if err != nil {
			conn.CloseNow()
		}
	}()

	if err := writeCtl(ctx, conn, wire.ClientHello{Op: wire.OpJoin, Slot: code.Slot}); err != nil {
		return nil, fmt.Errorf("opening receiver: sending join: %w", err)
	}
	paired, err := readCtl(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("opening receiver: reading paired: %w", err)
	}
	if err := expectOp(paired, wire.OpPaired); err != nil {
		return nil, fmt.Errorf("opening receiver: %w", err)
	}
	logger.DebugContext(ctx, "paired", slog.Uint64("slot", uint64(code.Slot)))

	key, err := wire.RecvHandshakeMsg(ctx, wsMsgConn{conn: conn}, code)
	if err != nil {
		return nil, fmt.Errorf("opening receiver: pake: %w", err)
	}
	logger.DebugContext(ctx, "pake key derived")

	ice := iceServersFromEnvelope(paired)
	if policy == RelayNone {
		ice = nil
	}
	cfg := rtc.Config{
		ICEServers:      ice,
		TransportPolicy: policy.transportPolicy(),
		Logger:          logger,
	}
	rs, err := rtc.Respond(ctx, wsMsgConn{conn: conn}, key, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening receiver: rtc respond: %w", err)
	}

	return startSession(rs, conn, key, logger, onTransfer), nil
}

// startSession wires a freshly-opened rtc.Session into a client.Session and
// kicks off the inbound pump.
func startSession(rs *rtc.Session, conn *websocket.Conn, key []byte, logger *slog.Logger, onTransfer SinkOpener) *Session {
	pumpCtx, pumpStop := context.WithCancel(context.Background())
	s := &Session{
		rtc:        rs,
		conn:       conn,
		key:        key,
		logger:     logger,
		onTransfer: onTransfer,
		pumpDone:   make(chan struct{}),
		pumpCtx:    pumpCtx,
		pumpStop:   pumpStop,
	}
	s.pumpWG.Go(func() {
		s.runPump()
	})
	return s
}

// Push streams one outbound transfer: writes preamble + payload, age-encrypted
// with the session key, terminated by tagEOF at the rtc layer. Returns once
// the rtc Push has finished writing the framing terminator. Not safe to call
// concurrently with itself.
func (s *Session) Push(ctx context.Context, preamble wire.Preamble, src io.Reader) error {
	pr, pw := io.Pipe()
	var eg errgroup.Group
	eg.Go(func() error {
		err := s.encodeTransfer(pw, preamble, src)
		_ = pw.CloseWithError(err)
		return err
	})
	if err := s.rtc.Push(ctx, pr); err != nil {
		_ = pr.Close()
		_ = eg.Wait()
		return fmt.Errorf("push: rtc: %w", err)
	}
	if err := eg.Wait(); err != nil {
		return fmt.Errorf("push: encode: %w", err)
	}
	return nil
}

// encodeTransfer writes one transfer's framing — age-encrypted preamble +
// payload — to w. Caller is responsible for closing w when this returns.
func (s *Session) encodeTransfer(w io.Writer, preamble wire.Preamble, src io.Reader) error {
	wc, err := wire.Encrypt(w, s.key)
	if err != nil {
		return fmt.Errorf("starting encrypt: %w", err)
	}
	if err := wire.WritePreamble(wc, preamble); err != nil {
		_ = wc.Close()
		return fmt.Errorf("writing preamble: %w", err)
	}
	if _, err := io.Copy(wc, src); err != nil {
		_ = wc.Close()
		return fmt.Errorf("copying payload: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("finalizing encrypt: %w", err)
	}
	return nil
}

// runPump loops on rtc.Pull, dispatching each inbound transfer through age
// decrypt + preamble parsing into the SinkOpener registered at open time.
// Exits when the rtc session ends (returns io.EOF) or onTransfer returns an
// error. The terminal error is recorded on pumpErr.
func (s *Session) runPump() {
	defer close(s.pumpDone)
	for {
		err := s.runOneInbound(s.pumpCtx)
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		s.pumpErr = err
		s.logger.DebugContext(s.pumpCtx, "session pump exit", slog.Any("err", err))
		return
	}
}

// runOneInbound consumes one rtc-level inbound transfer. Returns nil after
// the transfer's tagEOF; io.EOF if the rtc session has ended; otherwise a
// real error.
func (s *Session) runOneInbound(ctx context.Context) error {
	pr, pw := io.Pipe()
	pullErr := make(chan error, 1)
	go func() {
		err := s.rtc.Pull(ctx, pw)
		_ = pw.CloseWithError(err)
		pullErr <- err
	}()

	procErr := s.handleInbound(pr)
	pullCloseErr := <-pullErr

	if pullCloseErr != nil {
		if errors.Is(pullCloseErr, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("pull: %w", pullCloseErr)
	}
	if procErr != nil {
		return fmt.Errorf("decode: %w", procErr)
	}
	return nil
}

func (s *Session) handleInbound(r io.Reader) error {
	if s.onTransfer == nil {
		// Drain to satisfy rtc.Pull, then surface a structured error so the
		// pump exits cleanly.
		_, _ = io.Copy(io.Discard, r)
		return fmt.Errorf("inbound transfer with no SinkOpener registered")
	}
	pr, err := wire.Decrypt(r, s.key)
	if err != nil {
		return fmt.Errorf("starting decrypt: %w", err)
	}
	sink := &preambleSink{openSink: s.onTransfer}
	if _, err := io.Copy(sink, pr); err != nil {
		_ = sink.Close()
		return fmt.Errorf("copying payload: %w", err)
	}
	return sink.Close()
}

// Close tears down the session. Both peers must call Close — the rtc layer
// performs a synchronous teardown handshake on the WebSocket signaling
// channel that requires the peer to also close. Idempotent.
func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if err := s.rtc.Close(ctx); err != nil {
			s.closeErr = fmt.Errorf("close: rtc: %w", err)
		}
		s.pumpStop()
		s.pumpWG.Wait()
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
	})
	return s.closeErr
}

// PumpErr reports the terminal error from the inbound pump goroutine, or
// nil if it exited cleanly. Only meaningful after Close.
func (s *Session) PumpErr() error {
	<-s.pumpDone
	return s.pumpErr
}

// Route reports the transport ICE selected for this session — direct
// peer-to-peer or relayed through TURN. Available immediately after Open*.
func (s *Session) Route() rtc.Route {
	return s.rtc.Route()
}
