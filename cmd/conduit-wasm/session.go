//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall/js"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/wire"
)

// sessionRegistry holds open client.Session handles keyed by an opaque
// string ID we hand back to JS. JS calls subsequent push/close ops by ID;
// the Go side looks up the handle and dispatches.
type sessionRegistry struct {
	mu      sync.Mutex
	next    atomic.Int64
	handles map[string]*sessionHandle
}

type sessionHandle struct {
	sess     *client.Session
	cancel   context.CancelFunc
	ctx      context.Context //nolint:containedctx // tied to JS-side session lifetime
	closed   atomic.Bool
	logger   *slog.Logger
	bridge   *wasmBridge
	onError  js.Value
	onClosed js.Value
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{handles: make(map[string]*sessionHandle)}
}

func (r *sessionRegistry) put(h *sessionHandle) string {
	id := "s" + strconv.FormatInt(r.next.Add(1), 10)
	r.mu.Lock()
	r.handles[id] = h
	r.mu.Unlock()
	return id
}

func (r *sessionRegistry) get(id string) *sessionHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handles[id]
}

func (r *sessionRegistry) remove(id string) {
	r.mu.Lock()
	delete(r.handles, id)
	r.mu.Unlock()
}

// registerSession adds session-mode bindings to the existing conduit object.
// Distinct from register so the additive nature is obvious — v1 send/recv
// keeps working; the web client opts into sessions explicitly.
func (b *wasmBridge) registerSession(parent context.Context, exports js.Value) {
	exports.Set("openSender", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.openSenderJS(parent, args)
	}))
	exports.Set("openReceiver", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.openReceiverJS(parent, args)
	}))
	exports.Set("sessionPushFile", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionPushFileJS(args)
	}))
	exports.Set("sessionPushText", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionPushTextJS(args)
	}))
	exports.Set("sessionClose", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionCloseJS(args)
	}))
}

// openSenderJS(server, callbacks) → sessionId.
// callbacks fields (all optional):
//
//	onCode(code), onPaired(), onTransfer(preamble, uint8array),
//	onProgress(direction, doneBytes), onError(msg), onClosed().
func (b *wasmBridge) openSenderJS(parent context.Context, args []js.Value) any {
	if len(args) < 2 {
		return js.Null()
	}
	server := args[0].String()
	cbs := args[1]
	onCode := cbs.Get("onCode")
	onPaired := cbs.Get("onPaired")
	onTransfer := cbs.Get("onTransfer")
	onError := cbs.Get("onError")
	onClosed := cbs.Get("onClosed")

	ctx, cancel := context.WithCancel(parent)
	h := &sessionHandle{
		bridge:   b,
		logger:   b.logger,
		ctx:      ctx,
		cancel:   cancel,
		onError:  onError,
		onClosed: onClosed,
	}
	id := b.sessions.put(h)

	openSink := makeWasmSinkOpener(onTransfer)

	b.startOp(func() {
		sess, err := client.OpenSender(ctx, b.logger, server, client.RelayAuto, func(code string) {
			safeInvoke(onCode, code)
		}, openSink)
		if err != nil {
			safeInvoke(onError, err.Error())
			b.sessions.remove(id)
			cancel()
			return
		}
		h.sess = sess
		safeInvoke(onPaired)
	})
	return id
}

// openReceiverJS(server, code, callbacks) → sessionId.
func (b *wasmBridge) openReceiverJS(parent context.Context, args []js.Value) any {
	if len(args) < 3 {
		return js.Null()
	}
	server := args[0].String()
	codeStr := args[1].String()
	cbs := args[2]
	onPaired := cbs.Get("onPaired")
	onTransfer := cbs.Get("onTransfer")
	onError := cbs.Get("onError")
	onClosed := cbs.Get("onClosed")

	code, err := wire.ParseCode(codeStr)
	if err != nil {
		safeInvoke(onError, err.Error())
		return js.Null()
	}

	ctx, cancel := context.WithCancel(parent)
	h := &sessionHandle{
		bridge:   b,
		logger:   b.logger,
		ctx:      ctx,
		cancel:   cancel,
		onError:  onError,
		onClosed: onClosed,
	}
	id := b.sessions.put(h)

	openSink := makeWasmSinkOpener(onTransfer)

	b.startOp(func() {
		sess, err := client.OpenReceiver(ctx, b.logger, server, code, client.RelayAuto, openSink)
		if err != nil {
			safeInvoke(onError, err.Error())
			b.sessions.remove(id)
			cancel()
			return
		}
		h.sess = sess
		safeInvoke(onPaired)
	})
	return id
}

// sessionPushFileJS(sessionId, payload, filename, mimeType, onDone)
func (b *wasmBridge) sessionPushFileJS(args []js.Value) any {
	if len(args) < 5 {
		return js.Undefined()
	}
	id := args[0].String()
	payload := copyBytesFromJS(args[1])
	filename := args[2].String()
	mimeType := args[3].String()
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if filename == "" {
		filename = "payload.bin"
	}
	onDone := args[4]

	h := b.sessions.get(id)
	if h == nil {
		safeInvoke(onDone, "session not found")
		return js.Undefined()
	}

	preamble := wire.Preamble{
		Kind: wire.PreambleKindFile,
		Name: filename,
		Size: int64(len(payload)),
		MIME: mimeType,
	}
	b.startOp(func() {
		if err := h.sess.Push(h.ctx, preamble, bytes.NewReader(payload)); err != nil {
			safeInvoke(onDone, err.Error())
			return
		}
		safeInvoke(onDone, js.Null())
	})
	return js.Undefined()
}

// sessionPushTextJS(sessionId, text, onDone)
func (b *wasmBridge) sessionPushTextJS(args []js.Value) any {
	if len(args) < 3 {
		return js.Undefined()
	}
	id := args[0].String()
	text := args[1].String()
	onDone := args[2]

	h := b.sessions.get(id)
	if h == nil {
		safeInvoke(onDone, "session not found")
		return js.Undefined()
	}
	payload := []byte(text)
	preamble := wire.Preamble{
		Kind: wire.PreambleKindText,
		Size: int64(len(payload)),
		MIME: "text/plain; charset=utf-8",
	}
	b.startOp(func() {
		if err := h.sess.Push(h.ctx, preamble, bytes.NewReader(payload)); err != nil {
			safeInvoke(onDone, err.Error())
			return
		}
		safeInvoke(onDone, js.Null())
	})
	return js.Undefined()
}

// sessionCloseJS(sessionId)
func (b *wasmBridge) sessionCloseJS(args []js.Value) any {
	if len(args) < 1 {
		return js.Undefined()
	}
	id := args[0].String()
	h := b.sessions.get(id)
	if h == nil {
		return js.Undefined()
	}
	if !h.closed.CompareAndSwap(false, true) {
		return js.Undefined()
	}

	b.startOp(func() {
		defer b.sessions.remove(id)
		defer h.cancel()
		if h.sess != nil {
			if err := h.sess.Close(h.ctx); err != nil {
				safeInvoke(h.onError, err.Error())
			}
		}
		safeInvoke(h.onClosed)
	})
	return js.Undefined()
}

// makeWasmSinkOpener wraps a JS onTransfer(preamble, uint8array) callback in
// a SinkOpener that buffers payload bytes per-transfer, then fires the
// callback once the transfer ends. Buffering keeps the JS side simple and
// matches the v1 recv path's all-at-once delivery; revisit when transfers
// grow large enough to matter.
func makeWasmSinkOpener(onTransfer js.Value) client.SinkOpener {
	if onTransfer.Type() != js.TypeFunction {
		return nil
	}
	return func(p wire.Preamble) (io.WriteCloser, error) {
		buf := &bytes.Buffer{}
		return &wasmTransferSink{buf: buf, preamble: p, onTransfer: onTransfer}, nil
	}
}

type wasmTransferSink struct {
	buf        *bytes.Buffer
	preamble   wire.Preamble
	onTransfer js.Value
}

func (w *wasmTransferSink) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *wasmTransferSink) Close() error {
	preObj := js.Global().Get("Object").New()
	preObj.Set("kind", w.preamble.Kind)
	preObj.Set("name", w.preamble.Name)
	preObj.Set("size", w.preamble.Size)
	preObj.Set("mime", w.preamble.MIME)
	safeInvoke(w.onTransfer, preObj, uint8ArrayFromBytes(w.buf.Bytes()))
	return nil
}

var _ = fmt.Sprint // keep fmt available for inline diagnostics
