# conduit — session-mode transfers (persistent + bidirectional)

A planned successor to the v1 one-shot model. PLAN.md's architecture stands;
this doc only changes the post-handshake portion of the protocol and the
client-side state machines that consume it.

## Motivation

v1 commits to one transfer per code: a peer is *the sender* or *the
receiver*, the data channel tears down at tagEOF, and the slot is gone. The
two changes here:

- **Persistent channel.** After a transfer completes, both peers stay
  connected. They can do another transfer without re-pairing.
- **Bidirectional.** Either peer may initiate a transfer. The
  sender/receiver split disappears at the protocol level.

These collapse the v1 send/receive UX split into a single "open a conduit"
flow, matching webwormhole's mental model.

## Decisions

| Dimension                | Choice                                                                                                  |
| ------------------------ | ------------------------------------------------------------------------------------------------------- |
| Direction model          | Two unidirectional data channels per session, one per direction                                         |
| Concurrency              | Serialize transfers within a direction; allow concurrent across directions                              |
| Session boundary         | New: explicit goodbye op. Until then: connection persists and slot survives                             |
| Slot lifetime (server)   | Keep slot alive while either WebSocket is open; idle timeout matches v1 (10 min)                        |
| Wire teardown            | Move `exchangeTeardown` from per-transfer to session-close; per-transfer ends with tagEOF + ack drain only |
| Framing tags             | Unchanged: tagData / tagEOF / tagAck. Each transfer is preamble → tagData* → tagEOF + ack drain         |
| CLI default              | One-shot stays default (`send`, `recv` unchanged). New `conduit session [code]` enters session mode     |
| Web default              | Session mode (always)                                                                                   |

## Mental model

A *session* has three states:

```
        ┌───────────┐  pair + PAKE  ┌────────┐  goodbye  ┌────────┐
idle ──►│ dialling  │──────────────►│ paired │──────────►│ closed │
        └───────────┘                └────┬───┘           └────────┘
                                          │ either side starts a transfer
                                          ▼
                                    ┌──────────────┐
                                    │ transferring │
                                    └──────┬───────┘
                                           │ tagEOF + ack drain
                                           ▼
                                       (back to paired)
```

Both peers run the same state machine. There is no sender/receiver role —
just two peers in `paired`, either of whom can transition the session into
`transferring` (per direction).

## Transport

Two `RTCDataChannel`s, negotiated once during the v1 handshake:

| DC label   | Direction                | Carries                           |
| ---------- | ------------------------ | --------------------------------- |
| `a→b`      | peer A → peer B          | tagData / tagEOF / tagAck (acks for prior B→A traffic if any, but in practice this DC carries A's outbound payload + acks for inbound… see note) |
| `b→a`      | peer B → peer A          | symmetric                         |

**Note on ack multiplexing:** the v1 framing puts tagAck on the same DC as
the data it acknowledges, on the assumption that "only one peer writes each
tag class." With two unidirectional DCs, that assumption holds per-DC: each
DC has exactly one writer, who emits tagData/tagEOF for outbound transfers
and tagAck for inbound ones it's acknowledging. The framing.go invariant is
preserved.

**Peer labelling:** during PAKE, both sides derive the same K. We add a tie
break (e.g., compare ephemeral pubkeys) to assign A and B deterministically,
so each peer knows which DC it owns for outbound traffic. The label is
*not* a role — it just disambiguates DC ownership.

## Per-transfer lifecycle (within `paired`)

1. Initiator writes a fresh preamble (existing `wire.Preamble`, JSON,
   length-prefixed) onto its outbound DC.
2. Initiator streams payload as tagData frames; emits tagEOF on completion.
3. Acceptor's tagReader returns io.EOF on tagEOF; receiver-side preamble
   sink writes the file and emits tagAck frames on its outbound DC.
4. After tagEOF, initiator's `ackEG.Wait()` drains the corresponding
   inbound acks (existing barrier — load-bearing, unchanged).
5. Both sides return to `paired`. Neither DC closes.

The receive-side preamble sink, currently one-shot
(`internal/client/client.go:229-294`), becomes a loop that reads preamble →
opens sink → drains payload → repeats. New abstraction: a `Receiver` that
owns the inbound DC for the session's lifetime.

## Session close

A new wire op moves `exchangeTeardown` from per-transfer to session-level:

- Either peer issues `OpSessionClose` (over either DC, encrypted by the
  session key).
- Both peers run `exchangeTeardown` (same as v1, but now once per session
  not once per transfer), then close DCs and PC.
- Server observes the WebSocket close (or DC close, depending on whether
  signaling has stayed open) and removes the slot.

If a peer drops without sending `OpSessionClose`, the existing close
detection paths apply (PC ICE failure, DC close, WebSocket close). Idle
timeout on the server bounds zombie slots.

## Wire ops added

Only one new op for v1 of this redesign:

| Op                | Direction        | Payload         | Notes                                  |
| ----------------- | ---------------- | --------------- | -------------------------------------- |
| `OpSessionClose`  | DC, either way   | none            | Triggers the teardown handshake        |

We deliberately don't add an "I'm starting transfer N" op. The presence of
a fresh preamble after the previous transfer's ack-drain is implicit
signal enough. Receivers stay in a "expecting preamble" sub-state between
transfers.

## Server changes

`internal/signaling`:

- Slot lifetime no longer ends when the relay loop quiesces. Slots stay
  alive until both WebSockets close or the idle timer fires.
- Slot struct's `senderConn` / `receiverConn` rename to `peerA` / `peerB`.
  Server does not assign roles; it just relays. (This is mostly cosmetic —
  the relay is already opaque — but eliminates dead vocabulary.)
- `removeSlot` triggers on WebSocket close, not on relay-loop end.

`internal/turnauth`, `internal/ratelimit`: unchanged.

## Client changes

| File                           | Change                                                                |
| ------------------------------ | --------------------------------------------------------------------- |
| `internal/rtc/rtc.go`          | Replace `Send`/`Recv` with `Open` (creates/joins) returning a `Session`; move PC close from per-call defer to `Session.Close`; per-direction DC setup |
| `internal/rtc/framing.go`      | Unchanged (tags + reader/writer logic survives)                       |
| `internal/wire/wire.go`        | Add `OpSessionClose`; existing preamble survives                      |
| `internal/client/client.go`    | New `Session` type wrapping rtc.Session, exposing `Push(Source)` and a receive callback; preambleSink loops |
| `cmd/conduit/main.go`          | Keep `send` / `recv` as one-shot wrappers; add `session` subcommand   |
| `cmd/conduit-wasm/main.go`     | Replace `send` / `recv` exports with `open(code?) → SessionHandle`; expose `session.push(payload)` and `session.onTransfer(cb)` |
| `cmd/conduit-server/web/`      | Two-screen UX: idle (single phrase input + button) → connected (dropzone + paste + transfer list) |

## CLI ergonomics

```
conduit send <path>           # unchanged: one-shot, exits after transfer
conduit recv <code>           # unchanged: one-shot, exits after transfer

conduit session               # create session, print code, REPL stdin
conduit session <code>        # join session, REPL stdin
```

REPL mode: each line is a path to push. Empty line = wait. `Ctrl+D` = send
`OpSessionClose`. Incoming transfers are written to CWD with progress on
stderr.

This is workable but unusual for a Unix CLI; the alternative of a single
session-mode flag (`conduit send --keep-open`) is simpler if the REPL turns
out to feel awkward in practice. **Open question.**

## Web UX

| Phase     | Visible elements                                                                   |
| --------- | ---------------------------------------------------------------------------------- |
| `idle`    | One text input ("phrase"), one button ("Open"). Empty submit creates; filled joins |
| `paired`  | Dropzone + paste textarea + "send code" display (for the other peer to type) + transfer list |
| `closed`  | "Session ended" + "Open another" button                                            |

Either peer's UI is identical post-pair. The transfer list shows entries
labelled `↑` / `↓` matching webwormhole's convention.

## Risks / open questions

- **`ackEG.Wait` semantics under interleaved transfers.** Per-transfer it
  still drains correctly (one writer per DC means tagAck stream is
  unambiguous). What's untested is concurrent transfers in opposite
  directions — both directions running simultaneously means two ackEGs
  alive at once, one per Session direction. Should be fine but warrants a
  pion-level smoke test before relying on it.
- **NAT / SCTP keepalive.** SCTP heartbeats keep ICE alive on an idle PC,
  but a session with 30+ minutes of idle time may need application-level
  pings on long-lived TURN paths. Defer; monitor.
- **CLI REPL ergonomics.** May feel un-Unix. Alternative: drop the REPL,
  ship `--keep-open` instead, and let the user re-invoke with new args.
  Decide after a working web UX.
- **Backwards compatibility.** A v1 client opening a session-mode peer
  must still work for one-shot. Path: keep the v1 wire ops fully working,
  treat `OpSessionClose` as optional (peers that don't send it just drop
  the connection v1-style). Session features are then opt-in by both peers
  agreeing to stay paired.
- **Concurrent-transfer UX.** The transfer list can show two in-flight
  rows, one each direction. Whether to *expose* concurrency or pretend
  transfers are serialized is a UX decision; protocol allows both.

## Phased build

1. **rtc.Session prototype.** Two-DC handshake, idle/paired/transferring
   states in isolation. Existing framing reused. Two-CLI test harness.
2. **Wire op + signaling slot lifetime.** `OpSessionClose`; server stops
   removing slots on relay-quiesce.
3. **Client API.** `client.Session` with Push + receive callback;
   preambleSink → preambleLoop. CLI `session` subcommand wired up.
4. **WASM exports + web UI.** Idle/paired/transfer-list screens. Drop the
   v1 send/receive panels.
5. **Concurrent-direction stress test + backwards-compat check.** Prove
   v1 send + v1 recv still pair correctly with a v2 peer that opts out of
   session mode.

## Out of scope (still)

Everything PLAN.md's "Open / deferred" lists, plus:

- **Identity / persistent peers.** Sessions stay per-code-pair; no
  account, no contact list.
- **More than two peers.** No multicast / fan-out.
- **Resume across PC reconnect.** A dropped PC ends the session.
