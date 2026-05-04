# conduit

Encrypted file and text transfer between two peers. A short code pairs them through a signaling server; [SPAKE2](https://www.rfc-editor.org/rfc/rfc9382) turns that code into a shared session key the server never sees; [age](https://age-encryption.org) encrypts the payload, with zstd compression applied transparently when it pays off. Peers connect directly via WebRTC when possible; a TURN relay forwards opaque ciphertext when they can't.

## How it works

**Pairing.** The code is `<slot>-word-word-word`: the slot routes the connection on the server, and the word portion is the SPAKE2 password. `conduit send` reserves a slot, prints the full code, and waits. The receiver passes the code to `conduit recv`; the server binds the two sockets by slot and then only forwards bytes — it never reads them.

**Key establishment.** Both peers run SPAKE2 over the relay, using the word portion of the code as the password. This produces a shared 32-byte session key via HKDF. A passive observer (including the relay) sees only the SPAKE2 messages, which reveal nothing about the key. An active attacker intercepting the handshake would need to guess the word password before the transfer completes.

**Encryption.** The session key is used as an [age](https://age-encryption.org) scrypt passphrase. Everything that follows — ICE candidates, file metadata, file contents, progress acknowledgments — is encrypted before leaving either peer. The relay forwards ciphertext only; filenames and sizes are never visible to it.

**Compression.** Payloads are zstd-compressed before encryption when it's likely to help (tar streams, uncompressed file types) and skipped for already-compressed formats and stdin pipes where the codec overhead would outweigh the gain. The receiver decodes transparently, and progress meters report uncompressed bytes either way.

**Transport.** Peers exchange ICE candidates through the encrypted channel to attempt a direct WebRTC data channel. If NAT traversal succeeds they connect peer-to-peer; otherwise a TURN relay forwards the encrypted stream.

## Install

Pre-built binaries for Linux, macOS, and Windows are on the [GitHub Releases](https://github.com/danielmmetz/conduit/releases) page.

To install via Go (requires Go 1.26+):

```bash
go install github.com/danielmmetz/conduit/cmd/conduit@latest
```

To build from source:

```bash
go build -o conduit ./cmd/conduit
go build -o conduit-server ./cmd/conduit-server
```

## Usage

Pass `--server <URL>` (or set `CONDUIT_SERVER`) to use a different signaling host.

**Send** (prints a code; waits for the receiver):

```bash
conduit send --text 'hello'
conduit send ./myfile.bin
conduit send ./mydir              # streams as tar
conduit send a.txt b.txt c.txt   # multiple files → tar
tar c ./mydir | conduit send -   # stdin
```

**Receive**:

```bash
conduit recv '42-word-word-word'
conduit recv '42-word-word-word' -o out.bin
conduit recv '42-word-word-word' -o ./dest  # dir/tar extracts here
conduit recv '42-word-word-word' -          # stdout
```

**Watch** — stay paired across multiple transfers:

```bash
conduit recv --watch '42-word-word-word'  # joiner: stays connected through the peer's session
conduit recv --watch                       # host: holds a stable slot; any sender can reach it
```

## Web client

The signaling server also hosts a browser client (Go compiled to WASM). Visiting `https://conduit.danielmmetz.com/#<code>` pre-fills receive and starts a transfer; the same page can send files directly from a browser tab.

## Acknowledgments

**[webwormhole](https://github.com/saljam/webwormhole)** — the architecture conduit is built on: PAKE-derived session keys over a signaling server, WebRTC for the data channel, and the principle that the server should be structurally blind to transfer contents.

**[croc](https://github.com/schollz/croc)** — inspiration for CLI ergonomics and UX: short human-readable join codes, sensible send/receive defaults, and treating relay-forwarded transfer as a first-class path rather than a fallback.

## Docs

[`docs/`](docs/) covers server deployment, TURN configuration, and protocol notes.
