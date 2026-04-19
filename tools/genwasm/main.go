// Command genwasm copies wasm_exec.js from GOROOT and builds cmd/conduit-wasm
// into cmd/conduit-server/web/main.wasm. Invoked by go generate from
// cmd/conduit-server (see web_assets.go).
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := mainE(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintln(os.Stderr, "genwasm:", err)
		os.Exit(1)
	}
}

func mainE(ctx context.Context) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	root, err := findModuleRoot(wd)
	if err != nil {
		return err
	}
	goroot, err := goEnv(ctx, "GOROOT")
	if err != nil {
		return err
	}
	wasmExecSrc := filepath.Join(goroot, "lib", "wasm", "wasm_exec.js")
	webDir := filepath.Join(root, "cmd", "conduit-server", "web")
	if err := os.MkdirAll(webDir, 0o755); err != nil {
		return fmt.Errorf("creating web directory: %w", err)
	}
	if err := copyFile(wasmExecSrc, filepath.Join(webDir, "wasm_exec.js")); err != nil {
		return fmt.Errorf("copying wasm_exec.js: %w", err)
	}
	out := filepath.Join(webDir, "main.wasm")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/conduit-wasm")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building wasm: %w", err)
	}
	return nil
}

func findModuleRoot(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("finding module root: no go.mod from %s", dir)
		}
		dir = parent
	}
}

func goEnv(ctx context.Context, k string) (string, error) {
	out, err := exec.CommandContext(ctx, "go", "env", k).Output()
	if err != nil {
		return "", fmt.Errorf("go env %s: %w", k, err)
	}
	return string(bytes.TrimSpace(out)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copying to %s: %w", dst, err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dst, err)
	}
	return nil
}
