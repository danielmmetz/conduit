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
	logger *slog.Logger
	ops    sync.WaitGroup
}

func newWasmBridge(logger *slog.Logger) *wasmBridge {
	return &wasmBridge{logger: logger}
}

func (b *wasmBridge) register(ctx context.Context) {
	exports := js.Global().Get("Object").New()
	exports.Set("send", js.FuncOf(func(this js.Value, args []js.Value) any {
		return b.sendJS(ctx, this, args)
	}))
	exports.Set("recv", js.FuncOf(func(this js.Value, args []js.Value) any {
		return b.recvJS(ctx, this, args)
	}))
	js.Global().Set("conduit", exports)
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
	sendJSArgN = 6 // server, payload, filename, onCode, onProgress, onDone
	recvJSArgN = 4 // server, code, onProgress, onDone
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
		Kind: wire.PreambleKindFile,
		Name: filename,
		Size: int64(len(payload)),
		MIME: "application/octet-stream",
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
// onProgress(bytesReceived) is invoked as payload arrives;
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
	pw := &progressWriter{
		dst: &buf,
		cb: func(total int64) {
			safeInvoke(onProgress, total)
		},
	}
	var filename, kind, mimeType string
	open := func(pre wire.Preamble) (io.WriteCloser, error) {
		filename = pre.Name
		kind = pre.Kind
		mimeType = pre.MIME
		if pre.Kind == wire.PreambleKindTar {
			// Browser has nowhere to place extracted files; surface the tar
			// bytes as a single blob so the user can save it locally.
			filename = "conduit-received.tar"
		}
		return nopWriteCloser{pw}, nil
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
