# conduit

Encrypted file and text transfer between two online peers: a short **join code**, **WebRTC** for data when possible, and optional **TURN** relay. The relay server only pairs sockets and forwards opaque bytes—it never sees plaintext.

## Requirements

- [Go](https://go.dev/dl/) 1.26 or newer

## Build

From the repo root:

```bash
go build -o conduit ./cmd/conduit
go build -o conduit-server ./cmd/conduit-server
go build -o conduit-turn ./cmd/conduit-turn
```

Regenerate the embedded browser bundle (`wasm_exec.js` and `main.wasm` under `cmd/conduit-server/web/`) after changing `cmd/conduit-wasm`, or if those files are missing before you build the server:

```bash
go generate ./cmd/conduit-server
```

This runs `tools/genwasm`, which copies `wasm_exec.js` from `GOROOT` and cross-compiles the WASM binary. The server embeds `web/*` via `go:embed` (see `cmd/conduit-server/web_assets.go`).

## Run the signaling server

```bash
./conduit-server -addr :8080
```

Open the web UI at [http://localhost:8080](http://localhost:8080) (same port serves static assets and `GET /ws`). Health check: `GET /healthz` on the same origin (e.g. `http://localhost:8080/healthz`).

Optional TURN credential issuance (both CLI and web use it when present). You can either point clients at an external TURN service, or run TURN **inside** `conduit-server`:

**In-process TURN** (same binary; default UDP/TCP listeners `:3478`):

```bash
./conduit-server -addr :8080 \
  -turn-embed \
  -turn-public-ip '127.0.0.1'
```

With `-turn-embed`, if you omit `-turn-secret` the process generates a random 32-byte HMAC key at startup (issuer and embedded TURN stay in sync). Use an explicit `-turn-secret` when you need the same key after restarts or when using a separate `conduit-turn` process.

If you omit `-turn-uri`, the server advertises `turn:<turn-public-ip>:<port>?transport=udp` using the port from `-turn-listen-udp` (default `3478`). Set `-turn-uri` yourself when clients must use a hostname or non-default port.

**External TURN** (separate process or vendor): issue credentials and advertise where clients should connect:

```bash
./conduit-server -addr :8080 \
  -turn-secret 'a-long-random-secret' \
  -turn-uri 'turn:turn.example:3478?transport=udp'
```

For a standalone relay that matches `conduit-server` credentials, build `conduit-turn` (same `internal/turnserver` stack as `--turn-embed`).

## CLI usage

**Send** (prints a code; waits for the receiver):

```bash
./conduit send --server 'http://localhost:8080' --text 'hello'
./conduit send --server 'http://localhost:8080' ./myfile.bin
./conduit send --server 'http://localhost:8080' ./mydir              # streams as tar
./conduit send --server 'http://localhost:8080' a.txt b.txt c.txt    # multiple files → tar
tar c ./mydir | ./conduit send --server 'http://localhost:8080' -    # stdin
```

**Receive**:

```bash
./conduit recv --server 'http://localhost:8080' '42-word-word-word'
./conduit recv --server 'http://localhost:8080' -o out.bin '42-word-word-word'
./conduit recv --server 'http://localhost:8080' -o ./dest '42-word-word-word'  # dir/tar extracts here
./conduit recv --server 'http://localhost:8080' '42-word-word-word' -          # write to stdout
```

Without `-o`, files land at the sender's filename and directories extract into the current working directory.

Relay behavior:

- Default: try direct WebRTC, fall back to TURN if the server advertises it.
- `--no-relay`: strip TURN; fail if there is no direct path.
- `--force-relay`: ICE relay-only (useful for exercising TURN).

## Web client

The server embeds the SPA under `cmd/conduit-server/web/`. After `go generate ./cmd/conduit-server`, `go build ./cmd/conduit-server` picks up `main.wasm` and `wasm_exec.js`. The UI can **send** a file or **receive** using the same code format as the CLI; visiting `/#<code>` pre-fills receive and starts a transfer.

## What works today

- Rendezvous and blind relay over **WebSocket** (`/ws`)
- **SPAKE2** + **age** for session key and encrypted signaling/payload
- **WebRTC** data channel for the encrypted stream; **TURN** optional
- Payload shapes: single file, directory (streaming PAX tar), multi-file,
  stdin → stdout, with receiver→sender progress acks inside the encrypted stream
- **CLI** (`conduit`) and **browser** (Go→WASM + static JS) clients

See `PLAN.md` for design notes and the full roadmap.
