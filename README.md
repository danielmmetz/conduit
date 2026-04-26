# conduit

Encrypted file and text transfer between two online peers: a short **join code**, **WebRTC** for data when possible, and optional **TURN** relay. The relay server only pairs sockets and forwards opaque bytes—it never sees plaintext.

## Install

Pre-built binaries for Linux, macOS, and Windows are on the [GitHub Releases](https://github.com/danielmmetz/conduit/releases) page. Each archive ships `conduit` and `conduit-server`:

```bash
# Replace VERSION / OS / ARCH as needed (e.g. v0.1.0 / linux / amd64).
curl -LO https://github.com/danielmmetz/conduit/releases/download/VERSION/conduit_VERSION_OS_ARCH.tar.gz
tar -xzf conduit_VERSION_OS_ARCH.tar.gz
sudo install -m 0755 conduit conduit-server /usr/local/bin/
```

Verify against `checksums.txt` from the same release.

## Build from source

Requires [Go](https://go.dev/dl/) 1.26 or newer. From the repo root:

```bash
go build -o conduit ./cmd/conduit
go build -o conduit-server ./cmd/conduit-server
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

Optional TURN credential issuance via Cloudflare Realtime TURN (both CLI and web use it when present). Create a TURN key in the [Cloudflare Calls dashboard](https://dash.cloudflare.com/?to=/:account/calls), then:

```bash
./conduit-server -addr :8080 \
  -cloudflare-turn-key-id '<KEY_ID>' \
  -cloudflare-turn-api-token '<PER_KEY_API_TOKEN>'
```

The signaling server caches a credential per `--cloudflare-turn-ttl-seconds` (default 1h) and re-mints proactively at ~75% TTL, so the API is hit at most a few times per hour regardless of session rate. To plug in a different TURN provider, implement `turnauth.Issuer` and pass it via `signaling.WithTurnIssuer`.

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

## Deploy behind Caddy

`deploy/` contains example configs for a single-VPS deployment:

- [`deploy/Caddyfile`](deploy/Caddyfile) — TLS termination and reverse proxy for `/ws` and the embedded SPA.
- [`deploy/conduit-server.service`](deploy/conduit-server.service) — systemd unit running as a dedicated `conduit` user with the usual sandboxing. Reads `/etc/conduit/server.env` for `CONDUIT_SERVER_*` overrides (e.g. the Cloudflare TURN credentials).

Sketch (Cloudflare TURN):

```bash
sudo useradd --system --home /var/lib/conduit --shell /usr/sbin/nologin conduit
sudo install -d -o conduit -g conduit /etc/conduit
sudo tee /etc/conduit/server.env >/dev/null <<EOF
CONDUIT_SERVER_CLOUDFLARE_TURN_KEY_ID=<key id from CF Calls dashboard>
CONDUIT_SERVER_CLOUDFLARE_TURN_API_TOKEN=<per-key api token>
EOF
sudo chmod 0640 /etc/conduit/server.env
sudo chown root:conduit /etc/conduit/server.env

sudo install -m 0644 deploy/conduit-server.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now conduit-server
```

Point Caddy at `deploy/Caddyfile` (edit the hostname) and it handles TLS + the HTTP side.

Rate limits and the global concurrent-slot cap are tunable via `-reserve-per-min`, `-join-per-min`, and `-max-slots` (default `2000`). Raise `-max-slots` if you expect bursts of many idle reservations; lower it to cap memory on small hosts.

## What works today

- Rendezvous and blind relay over **WebSocket** (`/ws`)
- **SPAKE2** + **age** for session key and encrypted signaling/payload
- **WebRTC** data channel for the encrypted stream; **TURN** optional
- Payload shapes: single file, directory (streaming PAX tar), multi-file,
  stdin → stdout, with receiver→sender progress acks inside the encrypted stream
- **CLI** (`conduit`) and **browser** (Go→WASM + static JS) clients

See `PLAN.md` for design notes and the full roadmap.
