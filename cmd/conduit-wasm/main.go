//go:build js && wasm

// conduit-wasm is the browser entrypoint: it registers globalThis.conduit with
// send/recv methods that drive the shared internal/client transfer path.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"syscall/js"

	"github.com/danielmmetz/conduit/internal/client"
	"github.com/danielmmetz/conduit/internal/wire"
	"github.com/danielmmetz/conduit/internal/xfer"
	"rsc.io/qr"
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	rootCtx := context.Background()
	b := newWasmBridge(logger)
	b.register(rootCtx)
	select {}
}

// wasmBridge binds syscall/js callbacks to internal/client. Each async
// operation uses [sync.WaitGroup.Go] so goroutine lifetimes are tracked without
// manual Add/Done. The browser runtime does not stop the WASM module, so
// nothing calls Wait.
type wasmBridge struct {
	logger   *slog.Logger
	ops      sync.WaitGroup
	sessions *sessionRegistry
}

func newWasmBridge(logger *slog.Logger) *wasmBridge {
	return &wasmBridge{logger: logger, sessions: newSessionRegistry()}
}

func (b *wasmBridge) register(ctx context.Context) {
	exports := js.Global().Get("Object").New()
	exports.Set("send", js.FuncOf(func(this js.Value, args []js.Value) any {
		return b.sendJS(ctx, this, args)
	}))
	exports.Set("recv", js.FuncOf(func(this js.Value, args []js.Value) any {
		return b.recvJS(ctx, this, args)
	}))
	exports.Set("sendText", js.FuncOf(func(this js.Value, args []js.Value) any {
		return b.sendTextJS(ctx, this, args)
	}))
	exports.Set("qr", js.FuncOf(func(_ js.Value, args []js.Value) any {
		return qrJS(args)
	}))
	b.registerSession(ctx, exports)
	js.Global().Set("conduit", exports)
}

// qrJS(text) → {size, modules} where modules is a Uint8Array of length
// size*size with each byte 1 (dark) or 0 (light), row-major. Returns null on
// encode failure (e.g. text too long for the medium ECC level). The caller
// adds its own quiet zone when rendering.
func qrJS(args []js.Value) any {
	if len(args) < 1 {
		return js.Null()
	}
	code, err := qr.Encode(args[0].String(), qr.M)
	if err != nil {
		return js.Null()
	}
	out := make([]byte, code.Size*code.Size)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				out[y*code.Size+x] = 1
			}
		}
	}
	obj := js.Global().Get("Object").New()
	obj.Set("size", code.Size)
	obj.Set("modules", uint8ArrayFromBytes(out))
	return obj
}

// startOp runs f in a tracked goroutine. A panic inside f (including one
// raised by js.Value.Invoke when the JS callback throws) would otherwise
// terminate the whole wasm runtime — subsequent JS → Go calls would fail
// with "Go program has already exited". Recover, surface the crash to the
// browser console, and keep the runtime alive.
func (b *wasmBridge) startOp(f func()) {
	b.ops.Go(func() {
		defer func() {
			if r := recover(); r != nil {
				msg := fmt.Sprintf("conduit-wasm: panic: %v\n%s", r, debug.Stack())
				js.Global().Get("console").Call("error", msg)
			}
		}()
		f()
	})
}

// safeInvoke calls fn with args and swallows any JS exception it throws. The
// underlying [js.Value.Invoke] turns JS exceptions into Go panics; without
// this wrapper a buggy UI callback would kill the wasm runtime and break
// every subsequent send/recv. Exceptions are surfaced to the browser console
// so they're still debuggable.
func safeInvoke(fn js.Value, args ...any) {
	if fn.Type() != js.TypeFunction {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			js.Global().Get("console").Call("error", fmt.Sprintf("conduit-wasm: callback threw: %v", r))
		}
	}()
	fn.Invoke(args...)
}

const (
	sendJSArgN     = 6 // server, payload, filename, onCode, onProgress, onDone
	sendTextJSArgN = 5 // server, text, onCode, onProgress, onDone
	recvJSArgN     = 4 // server, code, onProgress, onDone
)

// sendJS(server, payload, filename, onCode, onProgress, onDone)
// onCode(code) when the slot is reserved; onProgress(done, total);
// onDone(err) where err is null on success.
func (b *wasmBridge) sendJS(parent context.Context, _ js.Value, args []js.Value) any {
	if len(args) < sendJSArgN {
		return js.Undefined()
	}
	server := args[0].String()
	payload := copyBytesFromJS(args[1])
	filename := args[2].String()
	onCode := args[3]
	onProgress := args[4]
	onDone := args[5]
	if onDone.Type() != js.TypeFunction {
		return js.Undefined()
	}

	b.startOp(func() {
		b.runSend(parent, server, payload, filename, onCode, onProgress, onDone)
	})
	return js.Undefined()
}

func (b *wasmBridge) runSend(ctx context.Context, server string, payload []byte, filename string, onCode, onProgress, onDone js.Value) {
	if filename == "" {
		filename = "payload.bin"
	}
	pr := &progressReader{
		r:     bytes.NewReader(payload),
		total: int64(len(payload)),
		onProgress: func(done, total int64) {
			safeInvoke(onProgress, done, total)
		},
	}
	preamble := wire.Preamble{
		Kind:        wire.PreambleKindFile,
		Name:        filename,
		Size:        int64(len(payload)),
		MIME:        "application/octet-stream",
		Compression: wire.PreambleCompressionNone,
	}
	err := client.Send(ctx, b.logger, server, client.RelayAuto, preamble, pr, func(code string) {
		safeInvoke(onCode, code)
	}, nil)
	if err != nil {
		safeInvoke(onDone, err.Error())
		return
	}
	safeInvoke(onDone, js.Null())
}

// sendTextJS(server, text, onCode, onProgress, onDone)
// Sends a UTF-8 text payload (wire.PreambleKindText) for receivers that
// display or save text shapes differently from binary files.
func (b *wasmBridge) sendTextJS(parent context.Context, _ js.Value, args []js.Value) any {
	if len(args) < sendTextJSArgN {
		return js.Undefined()
	}
	server := args[0].String()
	text := args[1].String()
	onCode := args[2]
	onProgress := args[3]
	onDone := args[4]
	if onDone.Type() != js.TypeFunction {
		return js.Undefined()
	}

	b.startOp(func() {
		b.runSendText(parent, server, text, onCode, onProgress, onDone)
	})
	return js.Undefined()
}

func (b *wasmBridge) runSendText(ctx context.Context, server, text string, onCode, onProgress, onDone js.Value) {
	payload := []byte(text)
	pr := &progressReader{
		r:     bytes.NewReader(payload),
		total: int64(len(payload)),
		onProgress: func(done, total int64) {
			safeInvoke(onProgress, done, total)
		},
	}
	preamble := wire.Preamble{
		Kind:        wire.PreambleKindText,
		Size:        int64(len(payload)),
		MIME:        "text/plain; charset=utf-8",
		Compression: wire.PreambleCompressionNone,
	}
	err := client.Send(ctx, b.logger, server, client.RelayAuto, preamble, pr, func(code string) {
		safeInvoke(onCode, code)
	}, nil)
	if err != nil {
		safeInvoke(onDone, err.Error())
		return
	}
	safeInvoke(onDone, js.Null())
}

// recvJS(server, code, onProgress, onDone)
// onProgress(bytesReceived, totalBytes) is invoked as payload arrives; total is
// the preamble-advertised payload size, or -1 for streaming sources (stdin/tar)
// where the size is not known up front.
// onDone(err, data, filename, kind, mime) where data is a Uint8Array of the
// payload, filename is the name advertised in the preamble (empty for text
// shapes), kind is the preamble kind ("file"/"tar"/"text") and mime is the
// advertised content type. The UI uses kind/mime to decide whether to render
// payloads inline or as a download.
func (b *wasmBridge) recvJS(parent context.Context, _ js.Value, args []js.Value) any {
	if len(args) < recvJSArgN {
		return js.Undefined()
	}
	server := args[0].String()
	codeStr := args[1].String()
	onProgress := args[2]
	onDone := args[3]
	if onDone.Type() != js.TypeFunction {
		return js.Undefined()
	}

	b.startOp(func() {
		b.runRecv(parent, server, codeStr, onProgress, onDone)
	})
	return js.Undefined()
}

func (b *wasmBridge) runRecv(ctx context.Context, server, codeStr string, onProgress, onDone js.Value) {
	code, err := wire.ParseCode(codeStr)
	if err != nil {
		safeInvoke(onDone, err.Error(), js.Null(), "", "", "")
		return
	}
	if len(code.Words) == 0 {
		safeInvoke(onDone, "code is missing the word portion", js.Null(), "", "", "")
		return
	}
	var buf bytes.Buffer
	var totalSize int64 = -1
	pw := &progressWriter{
		dst: &buf,
		cb: func(received int64) {
			safeInvoke(onProgress, received, totalSize)
		},
	}
	var filename, kind, mimeType string
	open := func(pre wire.Preamble) (io.WriteCloser, error) {
		filename = pre.Name
		kind = pre.Kind
		mimeType = pre.MIME
		totalSize = pre.Size
		if pre.Kind == wire.PreambleKindTar {
			// Browser has nowhere to place extracted files; surface the tar
			// bytes as a single blob so the user can save it locally.
			filename = "conduit-received.tar"
		}
		// WrapDecode is a no-op for compression == "none"; for "zstd" it
		// decodes wire bytes before they hit the buffer so the browser
		// receives the user's original payload, not the encoded stream.
		decoded, err := xfer.WrapDecode(nopWriteCloser{pw}, pre.Compression)
		if err != nil {
			return nil, fmt.Errorf("wrapping decoder: %w", err)
		}
		return decoded, nil
	}
	err = client.Recv(ctx, b.logger, server, code, client.RelayAuto, open)
	if err != nil {
		safeInvoke(onDone, err.Error(), js.Null(), "", "", "")
		return
	}
	ua := uint8ArrayFromBytes(buf.Bytes())
	safeInvoke(onDone, js.Null(), ua, filename, kind, mimeType)
}

// nopWriteCloser adapts a progressWriter to io.WriteCloser — no cleanup is
// needed in the browser because we buffer all bytes in memory.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func copyBytesFromJS(v js.Value) []byte {
	if v.IsNull() || v.IsUndefined() {
		return nil
	}
	n := v.Get("length").Int()
	b := make([]byte, n)
	js.CopyBytesToGo(b, v)
	return b
}

func uint8ArrayFromBytes(b []byte) js.Value {
	ua := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(ua, b)
	return ua
}

type progressReader struct {
	r          io.Reader
	n          int64
	total      int64
	onProgress func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 && p.onProgress != nil {
		p.n += int64(n)
		p.onProgress(p.n, p.total)
	}
	return n, err
}

type progressWriter struct {
	dst io.Writer
	n   int64
	cb  func(total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.dst.Write(b)
	if n > 0 && p.cb != nil {
		p.n += int64(n)
		p.cb(p.n)
	}
	return n, err
}
