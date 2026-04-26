# conduit — a small, secure data-shipping service

A website and CLI tool for sharing files or text between two devices, taking
heavy inspiration from [croc], [e2ecp], [webwormhole], and [age]:

- Single static Go binary CLI (croc).
- Zero-install browser client (webwormhole).
- Clients handle all encryption; the server never sees cleartext (all three).
- Prefer direct peer-to-peer, so bytes don't relay through us (webwormhole).
- Strong encryption with safe defaults, no knobs to get wrong (age).

[croc]: https://infinitedigits.co/croc/
[e2ecp]: https://infinitedigits.co/e2ecp/
[webwormhole]: https://github.com/saljam/webwormhole
[age]: https://github.com/FiloSottile/age

## Decisions

| Dimension          | Choice                                                                                                         |
| ------------------ | -------------------------------------------------------------------------------------------------------------- |
| Transport          | WebRTC P2P preferred, TURN relay fallback                                                                      |
| Sync model         | Synchronous only (both peers online)                                                                           |
| Crypto             | PAKE over short code → session key `K` → standard age ciphertext (via scrypt recipient, `K` as input, work=15) |
| Stack              | Go for server + CLI; Go→WASM for browser crypto                                                                |
| Metadata to server | Minimum viable; progress handled peer-to-peer inside the encrypted data channel                                |
| Code format        | `42-ice-cream-monkey` (numeric slot + 3 EFF-short words)                                                       |
| CLI shape          | `conduit send <path>`, `conduit recv <code>`                                                                   |
| Payloads           | single file, directories (streamed PAX tar), stdin/stdout text, multiple files                                 |
| Session policy     | Slots expire ~10 min idle; per-IP rate limits; no auth                                                         |
| Deployment         | Single VPS: Caddy + app; TURN handled by Cloudflare Realtime                                                   |
| Wordlist           | EFF short                                                                                                      |
| Distribution       | GitHub Releases                                                                                                |

## Architecture

```
      ┌─────────────────────────────── VPS ───────────────────────────────┐
      │                                                                   │
 ┌────┤  Caddy (TLS, static site, reverse proxy)                          │
 │    │    ├── serves /  (Go-WASM web client, static assets)              │
 │    │    └── proxies /ws → signaling server (WebSocket)                 │
 │    │                                                                   │
 │    │  signaling server (Go)                                            │
 │    │    - slot reservation, rate limiting                              │
 │    │    - blind message relay between two peers in a slot              │
 │    │    - mints short-lived TURN credentials (Cloudflare or HMAC)      │
 └────┘                                                                   │
      └───────────────────────────────────────────────────────────────────┘
                ▲                                 ▲
                │ WSS signaling                   │ WSS signaling
                │                                 │
           ┌────┴─────┐                     ┌─────┴────┐
           │  Sender  │◄═══ WebRTC SCTP ═══►│ Receiver │
           │ CLI/web  │  (direct, or TURN)  │ CLI/web  │
           └──────────┘            ▲        └──────────┘
                                   │
                          turn.cloudflare.com
```

## Protocol

### Slot reservation

1. Sender opens WSS, sends `{op: "reserve"}`. Server returns
   `{slot: 42, turn: {uri, username, credential, ttl}}`.
2. Sender generates PAKE secret (3 EFF-short words), displays full code
   `42-ice-cream-monkey` (+ QR + link in web UI).

### Rendezvous

3. Receiver opens WSS, sends `{op: "join", slot: 42}`. Server pairs the two
   sockets. Subsequent messages are relayed verbatim; server never parses them.

### Key agreement (SPAKE2, salt = slot id)

4. Sender sends `pake_msg1`; receiver sends `pake_msg2`; both derive
   `K = HKDF(SPAKE2_shared, "conduit/v1/session-key", 32)`.
5. If either side can't derive a matching key (wrong code, MITM), the next step
   fails closed.

### Signaling + encrypted data channel

6. Sender sends WebRTC SDP offer encrypted with `K`; receiver replies with SDP
   answer encrypted with `K`. This binds the WebRTC session to the PAKE
   transcript and hides SDP from the server.
7. ICE negotiation happens over the signaling socket. If direct connectivity
   succeeds, TURN is unused. Otherwise traffic flows through TURN (still
   ciphertext to the relay).

### Payload

Encryption uses age's stock scrypt recipient with `K` as the "passphrase" and
work factor 15. `K` already has 256 bits of entropy, so scrypt's stretching is
unnecessary for security — we use age/scrypt only to get a valid age file
format without writing a custom `age.Recipient`. Work factor 15 (rather than
1) sidesteps any future library-side minimum check at negligible cost (scrypt
runs once per file, not per chunk).

8. Over the data channel: an encrypted framing carries **preamble** (JSON:
   filenames, sizes, total bytes, MIME) → **payload** (age-encrypted stream;
   for directories, a PAX tar stream; for multiple files, PAX tar). Keyed
   with `K`.
9. Receiver periodically sends `{ack_bytes: N}` back so sender can show
   peer-side progress. Acks travel inside the same encrypted stream; the
   server (including TURN relay) never sees progress telemetry.
10. On completion: fsync, close channel, both sides print final status. Server
    logs connection close; no content metadata retained.

## Server components (one Go binary)

- `internal/signaling` — slot map (in-memory), WebSocket handlers, relay loop,
  expiry (ticker, default 10 min idle).
- `internal/turnauth` — `Issuer` interface and `CloudflareIssuer`, which
  mints short-lived credentials from Cloudflare Realtime TURN (via the Calls
  API) with caching and proactive re-mint.
- `internal/ratelimit` — token bucket per source IP (from `X-Forwarded-For` via
  Caddy), separate buckets for reserve/join.
- `cmd/conduit-server` — flags, config, graceful shutdown, `/healthz`
  (liveness: returns 200 if the slot map / accept loop are running).

## CLI (`cmd/conduit`)

```
conduit send <path...>        # one-or-more files, directory, or auto-tar
conduit send --text "hello"   # force text-mode
conduit send -                # read stdin
conduit recv <code>           # to CWD; preserves filename from preamble
conduit recv <code> -o <path> # override output
conduit recv <code> -         # write payload to stdout
```

Flags worth having from day one: `--server https://conduit.example`, `--no-relay`
(fail rather than use TURN), `-v/-q`, `--yes` (skip confirm on large /
directory receives).

Shared package `internal/wire` implements the protocol and is reused by both
the CLI and the WASM build.

## Web client

- Single static SPA. Dark/light auto. No build tooling beyond `esbuild` or
  plain JS.
- `main.wasm` (Go→WASM) exposes `send(file, onProgress)` and
  `recv(code, onProgress)` to JS.
- JS handles: file picker, drag-drop, QR rendering (qrcode.js), clipboard,
  progress UI.
- URL fragment support: visiting `/#42-ice-cream-monkey` pre-fills the receive
  field and auto-starts. Fragment never hits the server.
- Web Streams API + File System Access API where available, so the receiver
  can write directly to disk instead of buffering the full file in memory.

## Security / threat model (honest accounting)

### Protects against

- **Passive server.** Sees only slot ID + opaque PAKE bytes + encrypted SDP +
  TURN-relayed ciphertext. Cannot decrypt payload or learn filenames / sizes.
- **Active server / MITM.** Cannot complete PAKE without the short code;
  handshake fails closed. A MITM cannot forge a working session.
- **Offline brute-force of short code.** PAKE denies this — attacker must
  guess online, and per-slot attempts are single-use (wrong guess ends the
  session; rate limits bound retries).

### Does not protect against

- **An attacker who learns the short code** (shoulder-surf, over-loud phone
  call). Codes are single-use and expire; treat like a password.
- **Malicious peer.** If you give the code to the wrong person they get the
  file.
- **Tar traversal on directory receive.** Mitigation: receiver strips `..`,
  absolute paths, and symlinks before writing; refuses paths outside the
  chosen destination.
- **Compromise of the VPS.** Server sees no plaintext ever, but a compromised
  host can DoS, swap the WASM bundle to exfiltrate future payloads, or
  substitute the CLI download. Mitigations: sign release binaries; publish
  WASM hash; Subresource Integrity where possible.
- **Quantum adversary (future).** X25519 / SPAKE2 both fall. Out of scope
  for v1.

## Phased build

1. **Skeleton.** Repo layout, Go modules. Hello-world signaling WS.
2. **Rendezvous.** Slot reservation + relay + rate limiting. CLI `send`/`recv`
   stubs that just relay text.
3. **Crypto core.** SPAKE2 + age(scrypt, `K`). `internal/wire` round-trips
   ciphertext between two CLIs, no network.
4. **WebRTC.** pion data-channels + ICE + TURN credential issuance. Direct
   path only; verify P2P file copy.
5. **TURN fallback.** Cloudflare Realtime TURN credential issuer; force-relay
   flag to exercise the path.
6. **WASM + web UI.** Same `internal/wire` compiled to WASM; static SPA;
   QR + fragment link.
7. **Payload shapes.** Streaming tar for dirs, stdin/stdout, multi-file,
   preamble + progress acks.
8. **Harden + release.** Rate-limit tuning, release workflow
   (GoReleaser → GitHub Releases), install docs. First public release behind
   Caddy.

## Open / deferred

- **SPAKE2 library.** Pick an existing impl (e.g. `salsa.debian.org/vasudev/gospake2`
  or adapt `psanford/wormhole-william`'s PAKE) vs. write ~200 LoC from the
  spec. Audit both before choosing.
- **Resume / chunked-hash / fine-grained progress.** Deliberately deferred
  from v1.
- **Async drop-off / long-term age identities / multi-recipient.** Out of v1.
