# Deploying conduit-server

## Running the server

```bash
conduit-server -addr :8080
```

The same port serves the embedded SPA and the WebSocket endpoint (`GET /ws`). Health check: `GET /healthz`.

## TURN credentials (Cloudflare Realtime)

Both CLI and browser clients use TURN when the server advertises it. Create a TURN key in the [Cloudflare Calls dashboard](https://dash.cloudflare.com/?to=/:account/calls), then:

```bash
conduit-server -addr :8080 \
  -cloudflare-turn-key-id '<KEY_ID>' \
  -cloudflare-turn-api-token '<PER_KEY_API_TOKEN>'
```

The server caches one credential per `-cloudflare-turn-ttl-seconds` (default 1h) and re-mints proactively at ~75% TTL. To use a different TURN provider, implement `turnauth.Issuer` and pass it via `signaling.WithTurnIssuer`.

## Reverse proxy

[`deploy/Caddyfile`](../deploy/Caddyfile) has an example config for TLS termination and proxying `/ws` and the SPA.

## Tuning

Rate limits and the global slot cap are set via flags:

| Flag | Default | Effect |
|---|---|---|
| `-max-slots` | 2000 | Max concurrent open slots; raise for bursty workloads, lower to cap memory on small hosts |
| `-reserve-per-min` | — | Rate limit on slot reservations per source IP |
| `-join-per-min` | — | Rate limit on join attempts per source IP |

## Building the browser bundle

The server embeds the WASM client via `go:embed`. If `cmd/conduit-server/web/` is missing `main.wasm` or `wasm_exec.js` (or after editing `cmd/conduit-wasm`), regenerate:

```bash
go generate ./cmd/conduit-server
go build -o conduit-server ./cmd/conduit-server
```

`go generate` runs `tools/genwasm`, which copies `wasm_exec.js` from `GOROOT` and cross-compiles the WASM binary.
