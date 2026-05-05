package rtc_test

import (
	"context"
	"fmt"
)

// pipeConn is an in-memory, message-oriented wire.MsgConn pair. Each Send on
// one end becomes a single Recv on the other end. Buffered by one to avoid
// deadlock when the sender fires before the peer is ready.
type pipeConn struct {
	incoming chan []byte
	outgoing chan []byte
}

func newPipeConn() (*pipeConn, *pipeConn) {
	a2b := make(chan []byte, 4)
	b2a := make(chan []byte, 4)
	return &pipeConn{incoming: b2a, outgoing: a2b}, &pipeConn{incoming: a2b, outgoing: b2a}
}

func (p *pipeConn) Send(ctx context.Context, data []byte) error {
	buf := make([]byte, len(data))
	copy(buf, data)
	select {
	case p.outgoing <- buf:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("sending on test pipe: %w", ctx.Err())
	}
}

func (p *pipeConn) Recv(ctx context.Context) ([]byte, error) {
	select {
	case m := <-p.incoming:
		return m, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("receiving on test pipe: %w", ctx.Err())
	}
}
