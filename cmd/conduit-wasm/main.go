//go:build js && wasm

// conduit-wasm is the browser entrypoint: it registers globalThis.conduit with
// session-mode methods that drive the shared internal/client transfer path.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"syscall/js"

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
