//go:build js && wasm

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
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
	exports.Set("sessionPushTar", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionPushTarJS(args)
	}))
	exports.Set("sessionPushText", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionPushTextJS(args)
	}))
	exports.Set("sessionClose", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return b.sessionCloseJS(args)
	}))
}

// openSenderJS(server, callbacks) → sessionId.
// callbacks fields:
//
//	onCode(code)                          — slot reserved
//	onPaired(route)                       — peer attached; route is
//	                                        "direct", "relayed", or "unknown"
//	onTransferStart(preamble)             — inbound transfer beginning
//	onTransferProgress(doneBytes, total)  — bytes accumulated; total may be -1
//	onTransferEnd(preamble, uint8array)   — inbound transfer complete
//	onError(msg), onClosed()
//
// onTransferEnd carries the payload. onTransferStart and onTransferProgress
// let the receiver render a live row instead of waiting silently for the
// transfer to finish.
func (b *wasmBridge) openSenderJS(parent context.Context, args []js.Value) any {
	if len(args) < 2 {
		return js.Null()
	}
	server := args[0].String()
	cbs := args[1]
	onCode := cbs.Get("onCode")
	onPaired := cbs.Get("onPaired")
	onTransferStart := cbs.Get("onTransferStart")
	onTransferProgress := cbs.Get("onTransferProgress")
	onTransferEnd := cbs.Get("onTransferEnd")
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

	openSink := makeWasmSinkOpener(onTransferStart, onTransferProgress, onTransferEnd)

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
		safeInvoke(onPaired, sess.Route().String())
		b.watchPeerClose(h, id)
	})
	return id
}

// openReceiverJS(server, code, callbacks) → sessionId. Callbacks mirror
// openSenderJS minus onCode (the receiver supplies the code).
func (b *wasmBridge) openReceiverJS(parent context.Context, args []js.Value) any {
	if len(args) < 3 {
		return js.Null()
	}
	server := args[0].String()
	codeStr := args[1].String()
	cbs := args[2]
	onPaired := cbs.Get("onPaired")
	onTransferStart := cbs.Get("onTransferStart")
	onTransferProgress := cbs.Get("onTransferProgress")
	onTransferEnd := cbs.Get("onTransferEnd")
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

	openSink := makeWasmSinkOpener(onTransferStart, onTransferProgress, onTransferEnd)

	b.startOp(func() {
		sess, err := client.OpenReceiver(ctx, b.logger, server, code, client.RelayAuto, openSink)
		if err != nil {
			safeInvoke(onError, err.Error())
			b.sessions.remove(id)
			cancel()
			return
		}
		h.sess = sess
		safeInvoke(onPaired, sess.Route().String())
		b.watchPeerClose(h, id)
	})
	return id
}

// watchPeerClose blocks until the inbound pump exits, then fires onClosed
// — the peer terminated the session from their side (CLI recv that
// finished and closed, browser tab that ended the session, network drop
// on the other side, etc.). Without this watcher the JS UI stays in the
// paired state and any subsequent push fails with a confusing "write to
// closed dc" error rather than the clean "session ended" transition the
// state machine expects.
func (b *wasmBridge) watchPeerClose(h *sessionHandle, id string) {
	if h.sess == nil {
		return
	}
	// PumpErr blocks on pumpDone — fires once the rtc.Session demuxer
	// surfaces io.EOF (peer closed) or any other terminal error.
	_ = h.sess.PumpErr()
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	defer b.sessions.remove(id)
	defer h.cancel()
	// Best-effort local close: the rtc layer's teardown handshake will
	// short-timeout because the peer is already gone.
	_ = h.sess.Close(h.ctx)
	safeInvoke(h.onClosed)
}

// sessionPushFileJS(sessionId, payload, filename, mimeType, onProgress, onDone)
func (b *wasmBridge) sessionPushFileJS(args []js.Value) any {
	if len(args) < 6 {
		return js.Undefined()
	}
	id := args[0].String()
	payload := copyBytesFromJS(args[1])
	filename := args[2].String()
	mimeType := args[3].String()
	onProgress := args[4]
	onDone := args[5]
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	if filename == "" {
		filename = "payload.bin"
	}

	h := b.sessions.get(id)
	if h == nil {
		safeInvoke(onDone, "session not found")
		return js.Undefined()
	}
	if h.sess == nil {
		safeInvoke(onDone, "session not yet open")
		return js.Undefined()
	}

	preamble := wire.Preamble{
		Kind: wire.PreambleKindFile,
		Name: filename,
		Size: int64(len(payload)),
		MIME: mimeType,
	}
	src := wrapWithProgress(bytes.NewReader(payload), int64(len(payload)), onProgress)
	b.startOp(func() {
		if err := h.sess.Push(h.ctx, preamble, src); err != nil {
			safeInvoke(onDone, err.Error())
			return
		}
		safeInvoke(onDone, js.Null())
	})
	return js.Undefined()
}

// wrapWithProgress wraps r in a progressReader that fires onProgress
// (done, total) per Read when onProgress is a JS function. Returns r
// unchanged otherwise so callers needn't branch on the JS side.
func wrapWithProgress(r io.Reader, total int64, onProgress js.Value) io.Reader {
	if onProgress.Type() != js.TypeFunction {
		return r
	}
	return &progressReader{
		r:     r,
		total: total,
		onProgress: func(done, total int64) {
			safeInvoke(onProgress, done, total)
		},
	}
}

// sessionPushTarJS(sessionId, entries, displayName, onProgress, onDone)
//
// entries is a JS array of { name: string, data: Uint8Array }, where name is
// the slash-separated relative path and data is the file contents. Builds a
// PAX tar in memory (matching the CLI's directory-send shape) and pushes it
// as a single kind=tar transfer. Directory entries are inferred from the
// path prefixes; the receiver creates them on extract.
func (b *wasmBridge) sessionPushTarJS(args []js.Value) any {
	if len(args) < 5 {
		return js.Undefined()
	}
	id := args[0].String()
	entriesJS := args[1]
	displayName := args[2].String()
	onProgress := args[3]
	onDone := args[4]

	h := b.sessions.get(id)
	if h == nil {
		safeInvoke(onDone, "session not found")
		return js.Undefined()
	}
	if h.sess == nil {
		safeInvoke(onDone, "session not yet open")
		return js.Undefined()
	}
	if displayName == "" {
		displayName = "files.tar"
	}

	n := entriesJS.Length()
	type tarEntry struct {
		name string
		data []byte
	}
	entries := make([]tarEntry, 0, n)
	for i := range n {
		e := entriesJS.Index(i)
		name := e.Get("name").String()
		if name == "" {
			safeInvoke(onDone, "tar entry missing name")
			return js.Undefined()
		}
		entries = append(entries, tarEntry{name: name, data: copyBytesFromJS(e.Get("data"))})
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := make(map[string]bool)
	for _, e := range entries {
		// Materialize parent directory entries (in path order, deduped) so the
		// receiver's traversal-safe extractor can mkdir them via tar.TypeDir
		// rather than only seeing leaf-file entries that lack mode bits.
		for _, dir := range parentDirs(e.name) {
			if dirs[dir] {
				continue
			}
			dirs[dir] = true
			if err := tw.WriteHeader(&tar.Header{
				Name:     dir + "/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
				Format:   tar.FormatPAX,
			}); err != nil {
				safeInvoke(onDone, fmt.Sprintf("tar header %s: %v", dir, err))
				return js.Undefined()
			}
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.name,
			Typeflag: tar.TypeReg,
			Mode:     0o644,
			Size:     int64(len(e.data)),
			Format:   tar.FormatPAX,
		}); err != nil {
			safeInvoke(onDone, fmt.Sprintf("tar header %s: %v", e.name, err))
			return js.Undefined()
		}
		if _, err := tw.Write(e.data); err != nil {
			safeInvoke(onDone, fmt.Sprintf("tar body %s: %v", e.name, err))
			return js.Undefined()
		}
	}
	if err := tw.Close(); err != nil {
		safeInvoke(onDone, fmt.Sprintf("tar close: %v", err))
		return js.Undefined()
	}

	preamble := wire.Preamble{
		Kind: wire.PreambleKindTar,
		Name: displayName,
		Size: int64(buf.Len()),
		MIME: "application/x-tar",
	}
	src := wrapWithProgress(bytes.NewReader(buf.Bytes()), int64(buf.Len()), onProgress)
	b.startOp(func() {
		if err := h.sess.Push(h.ctx, preamble, src); err != nil {
			safeInvoke(onDone, err.Error())
			return
		}
		safeInvoke(onDone, js.Null())
	})
	return js.Undefined()
}

// parentDirs returns the chain of ancestor directories of a slash-separated
// path, in root-first order. parentDirs("a/b/c.txt") → ["a", "a/b"].
func parentDirs(path string) []string {
	parts := strings.Split(path, "/")
	if len(parts) <= 1 {
		return nil
	}
	out := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return out
}

// sessionPushTextJS(sessionId, text, onProgress, onDone)
func (b *wasmBridge) sessionPushTextJS(args []js.Value) any {
	if len(args) < 4 {
		return js.Undefined()
	}
	id := args[0].String()
	text := args[1].String()
	onProgress := args[2]
	onDone := args[3]

	h := b.sessions.get(id)
	if h == nil {
		safeInvoke(onDone, "session not found")
		return js.Undefined()
	}
	if h.sess == nil {
		safeInvoke(onDone, "session not yet open")
		return js.Undefined()
	}
	payload := []byte(text)
	preamble := wire.Preamble{
		Kind: wire.PreambleKindText,
		Size: int64(len(payload)),
		MIME: "text/plain; charset=utf-8",
	}
	src := wrapWithProgress(bytes.NewReader(payload), int64(len(payload)), onProgress)
	b.startOp(func() {
		if err := h.sess.Push(h.ctx, preamble, src); err != nil {
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

// makeWasmSinkOpener wraps three JS callbacks into a SinkOpener that
// surfaces an inbound transfer's lifecycle to the browser:
//
//   - onStart(preamble) fires once, as soon as the preamble is decoded.
//     The JS side uses this to render the transfer row immediately so the
//     receiver sees the filename and size before any bytes accumulate.
//   - onProgress(done, total) fires per Write as bytes arrive — one event
//     per rtc.Pull buffer (typically 32 KiB). The JS side throttles its
//     rendering; we pass every event so JS can decide.
//   - onEnd(preamble, uint8array) fires once when the transfer's tagEOF
//     terminates the stream, with the full payload buffered in memory.
//     The browser receiver consumes the bytes via blob/service-worker
//     download or inline rendering.
//
// onEnd is required (it carries the payload); onStart and onProgress are
// optional. The legacy single-callback API was replaced because it left
// the receiver staring at a blank panel for the duration of any
// non-trivial transfer.
func makeWasmSinkOpener(onStart, onProgress, onEnd js.Value) client.SinkOpener {
	if onEnd.Type() != js.TypeFunction {
		return nil
	}
	return func(p wire.Preamble) (io.WriteCloser, error) {
		if onStart.Type() == js.TypeFunction {
			safeInvoke(onStart, preambleObj(p))
		}
		return &wasmTransferSink{
			buf:        &bytes.Buffer{},
			preamble:   p,
			onProgress: onProgress,
			onEnd:      onEnd,
		}, nil
	}
}

type wasmTransferSink struct {
	buf        *bytes.Buffer
	preamble   wire.Preamble
	onProgress js.Value
	onEnd      js.Value
	n          int64
}

func (w *wasmTransferSink) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p)
	if n > 0 && w.onProgress.Type() == js.TypeFunction {
		w.n += int64(n)
		safeInvoke(w.onProgress, w.n, w.preamble.Size)
	}
	return n, err
}

func (w *wasmTransferSink) Close() error {
	safeInvoke(w.onEnd, preambleObj(w.preamble), uint8ArrayFromBytes(w.buf.Bytes()))
	return nil
}

// preambleObj converts a wire.Preamble into the JS object the session
// callbacks consume. Centralised so onStart and onEnd produce identical
// shapes — the JS state machine relies on that.
func preambleObj(p wire.Preamble) js.Value {
	obj := js.Global().Get("Object").New()
	obj.Set("kind", p.Kind)
	obj.Set("name", p.Name)
	obj.Set("size", p.Size)
	obj.Set("mime", p.MIME)
	return obj
}

