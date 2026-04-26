//go:build js && wasm

package rtc

import (
	"fmt"
	"io"
	"sync"

	"github.com/pion/webrtc/v4"
)

// Browsers cap RTCDataChannel send queues at an undocumented ceiling
// (Chromium historically allowed roughly 16 MiB before send() throws "send
// queue is full"); spec-wise no portable upper bound exists. The CLI's
// native pion stack applies SCTP-level backpressure inside Write, but the
// JS wrapper just forwards each Send call to the browser, so the payload
// pump in rtc.Send must stop when the queue is too full.
//
// The high/low watermarks below keep enough bytes in flight to saturate a
// fast link without ever approaching any browser's hard ceiling. They are
// also small enough to drain quickly on slow uplinks so the bufferedAmountLow
// event keeps firing.
const (
	bpHighWatermark uint64 = 1 << 20  // 1 MiB
	bpLowWatermark  uint64 = 256 << 10 // 256 KiB
)

// wrapSendWriter returns a writer that defers each underlying Write until
// dc.bufferedAmount drops below bpHighWatermark, using bufferedamountlow as
// the wakeup signal. Closing the data channel also wakes any blocked writer
// so the loop can observe the new state and surface an error.
func wrapSendWriter(dc *webrtc.DataChannel, w io.Writer) io.Writer {
	bp := &backpressureWriter{w: w, dc: dc}
	bp.cond = sync.NewCond(&bp.mu)
	dc.SetBufferedAmountLowThreshold(bpLowWatermark)
	dc.OnBufferedAmountLow(bp.signal)
	dc.OnClose(bp.signal)
	return bp
}

type backpressureWriter struct {
	w  io.Writer
	dc *webrtc.DataChannel

	mu   sync.Mutex
	cond *sync.Cond
}

func (b *backpressureWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	for {
		state := b.dc.ReadyState()
		if state != webrtc.DataChannelStateOpen {
			b.mu.Unlock()
			return 0, fmt.Errorf("backpressure: data channel state %s", state)
		}
		if b.dc.BufferedAmount() <= bpHighWatermark {
			break
		}
		b.cond.Wait()
	}
	b.mu.Unlock()
	return b.w.Write(p)
}

// signal is invoked from pion's bufferedamountlow / close callbacks (each
// dispatched on its own goroutine). Broadcasting under the lock guarantees
// the writer's loop re-reads bufferedAmount and ReadyState after the wakeup.
func (b *backpressureWriter) signal() {
	b.mu.Lock()
	b.cond.Broadcast()
	b.mu.Unlock()
}
