# Trickle ICE — context for a future attempt

A previous attempt to add trickle ICE was reverted because the resulting test
suite was flaky (~20% deadlock rate on loopback). This document captures
what trickle is, what it would buy, and the gotchas that bit the first
attempt — so the next try doesn't relearn them.

## What it is

In the standard WebRTC offer/answer flow there are two ways to ship ICE
candidates between peers:

- **Non-trickle** (current). The offerer calls `SetLocalDescription`, waits
  for ICE gathering to reach `complete`, then sends the SDP — which now
  contains every candidate inline as `a=candidate:` lines. The answerer
  does the same. The signaling channel only carries SDP.

- **Trickle**. The offerer sends the SDP **immediately** after
  `SetLocalDescription`, with no candidates inline. As each local
  candidate is gathered (`OnICECandidate` fires), it's sent to the peer
  over the same signaling channel. A `nil` candidate (end-of-gathering)
  is forwarded as an explicit "end-of-candidates" signal so the peer's
  agent can finalize.

The SDP signals trickle support via `a=ice-options:trickle`. Pion includes
that automatically.

## What it buys conduit

The current code uses a 3s gather deadline (internal/rtc/rtc.go's
`waitGather`) as a workaround for the 40s "wait for unreachable Cloudflare
:53 / IPv6 STUN endpoints to time out" problem. Trickle obsoletes the
deadline entirely:

| Path                      | Non-trickle (full gather) | Gather deadline (today) | Trickle |
|---------------------------|---------------------------|-------------------------|---------|
| SDP sent after            | ~40s                      | ≤3s                     | <100ms  |
| Cross-NAT pairing total   | ~80s                      | ~6s                     | ~1–2s   |

Same-machine pairings would feel instant. Cross-network would feel
snappy. The gather deadline is a clamp on a behavior we shouldn't have to
clamp; trickle removes the underlying need.

It also matches the pattern every browser uses natively, and what
webwormhole's WASM client does. Fewer surprises long-term.

## Wire protocol

Extend `signalMsg` (internal/rtc/rtc.go) with the WebRTC-spec candidate
fields, mirroring `webrtc.ICECandidateInit`:

```go
type signalMsg struct {
    Type             string  `json:"type"`
    SDP              string  `json:"sdp,omitempty"`
    Candidate        string  `json:"candidate,omitempty"`
    SDPMid           *string `json:"sdpMid,omitempty"`
    SDPMLineIndex    *uint16 `json:"sdpMLineIndex,omitempty"`
    UsernameFragment *string `json:"usernameFragment,omitempty"`
}
```

Two new `Type` values: `"ice-candidate"` (with the candidate fields
populated) and `"end-of-candidates"` (all fields empty). `pion.ICECandidate.ToJSON()` returns an `ICECandidateInit` whose fields map 1:1.

The encryption of these messages is the same as for SDP (`wire.Encrypt`
with the PAKE-derived key).

## Lifecycle

Three roles, three call sites (rtc.Send, rtc.Recv, rtc.openSession), but
the trickle pattern is identical. Worth factoring into a `trickleManager`
type from the start.

```
┌─ newPeerConnection
├─ pc.OnICECandidate(handler)           # register BEFORE SetLocalDescription
│                                       # handler queues until "ready"
├─ exchangeSDP                          # no waitGather; send SDP immediately
│   ├─ initiator: createOffer / setLocal / sendSignal(offer) / recvSignal(answer) / setRemote
│   └─ responder: recvSignal(offer) / setRemote / createAnswer / setLocal / sendSignal(answer)
├─ tm.start()                           # flush queued candidates, launch reader
│
│   <application phase: DC opens, transfer happens>
│
├─ tm.exchangeTeardown(ctx)             # bidirectional teardown over same reader
└─ tm.close()                           # cancel and join the reader
```

Order is load-bearing:

- **`OnICECandidate` must be registered before `SetLocalDescription`**.
  Pion starts gathering inside `SetLocalDescription`; candidates can fire
  from the gatherer goroutine before the call returns.

- **Local candidates that fire before the SDP exchange completes must be
  queued, not sent**. If you send a trickled candidate before the peer
  has set the corresponding remote description, pion's `AddICECandidate`
  on the peer side returns `InvalidStateError`.

- **`pc.AddICECandidate` on the local side requires the remote
  description to already be set**. So the trickle reader cannot start
  consuming peer candidates until after `exchangeSDP` returns.

- **End-of-candidates propagation**: when `OnICECandidate(nil)` fires
  locally, send `{Type: "end-of-candidates"}` to the peer. When the peer
  sends one to you, call
  `pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: ""})` — that
  reaches `iceTransport.AddRemoteCandidate(nil)` and lets pion's agent
  finalize the remote-candidate set. Skipping this step deadlocks the
  agent waiting for more candidates.

## Single-reader rule

The signaling channel `sig` is used for three things across a session
lifecycle: SDP exchange, trickle candidates, and the final teardown
handshake. **Only one goroutine may call `sig.Recv` at a time** —
WebSocket reads are not safely concurrent, and even a buffered pipe
deadlocks if two goroutines race for the next message.

The first attempt got this right by consolidating both trickle and
teardown into a single reader on a `trickleManager`. The reader's
dispatch:

```go
case "ice-candidate":      AddICECandidate(...)
case "end-of-candidates":   AddICECandidate({Candidate: ""}); keep reading
case teardownSignal:        close(teardownCh); return
```

`exchangeTeardown` becomes a method on `trickleManager`: it sends its
teardown signal via the same `sendMu` that serializes outbound trickle
candidates, then waits on `teardownCh`.

## Pion-specific gotchas

- **WASM `OnICECandidate` is async-dispatched**. Pion's WASM bindings
  call your handler via `go f(candidate)` (peerconnection_js.go:344), so
  the order in which you observe candidates may not match the order they
  were gathered. Do not rely on the `nil` (end-of-gathering) being
  observed last on the WASM build. The native build does call
  synchronously.

- **`pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: ""})`** is
  the canonical way to pass end-of-candidates through to pion. The
  native build accepts it; the WASM build no-ops it harmlessly.

- **Outbound sends must be serialized**. Multiple `OnICECandidate`
  invocations can race each other and the teardown send. A `sendMu` on
  the trickle manager — held during each `sendSignal` — is sufficient.

- **`OnICECandidate` continues firing after the data channel opens**.
  Don't tie its lifetime to the application phase; tie it to
  `tm.close()`. Inside `handleLocal`, gate sends on whether the manager
  has been closed, so a late candidate firing after teardown doesn't
  blast a stray frame onto a closed pipe.

## Why the first attempt was flaky

The signature: `TestSendRecvRoundTrip` deadlocked ~20% of runs with
both peers stuck in `datachanWait.wait` and DTLS hung mid-handshake.
Candidates were demonstrably flowing on the wire and `AddICECandidate`
wasn't returning errors. ICE had nominated *something*, because DTLS
had started, but the nominated pair didn't actually carry packets.

The most plausible cause is the **candidate cardinality**. In
non-trickle, the SDP arrives at the peer with all candidates at once,
and pion processes them in a single `SetRemoteDescription` operation.
With trickle, candidates flow asynchronously into the agent's
candidate-pair check loop while the agent is also nominating. With
~12 local candidates per side (docker bridges, Tailscale ULAs, IPv6
GUAs, link-local, loopback, LAN — typical of a developer laptop),
that's 144 pairs, and we're feeding them in faster than pion's
default checking interval can keep up.

Things to try next time:

1. **`SettingEngine.SetInterfaceFilter`** — exclude docker bridges and
   tunnel interfaces. Browsers do this implicitly; native pion gathers
   everything by default. Would also help non-trickle reliability.

2. **`SettingEngine.SetNetworkTypes`** to a smaller set (e.g.
   `NetworkTypeUDP4` only for tests) to bound the pair count.

3. **Don't propagate peer's end-of-candidates to local pc**. The first
   attempt tried both with and without; "with" was somewhat more
   reliable but neither was clean. Worth testing this against a
   filtered candidate set rather than the laptop's full kitchen sink.

4. **Add an ICE-connected gate** before considering trickle "done".
   Currently the manager's reader runs until the WS closes; a future
   version could exit the inner-loop early once
   `OnICEConnectionStateChange("connected")` fires, on the theory that
   late candidates can be ignored.

5. **Test pairs of pion peers across actual loopback** (not just
   `pipeConn` over channels) to surface whether the flake is in
   pion's agent or in our wire layer.

## Reference

- Pion's trickle example: pkg/mod/github.com/pion/webrtc/v4@v4.2.11/examples/trickle-ice/main.go
  Notably: the example just `return`s on `OnICECandidate(nil)` — it
  does **not** propagate end-of-candidates to the peer. That works for
  a one-off demo where ICE eventually times itself out into a working
  state; it's not what we want for a credential-sensitive flow where
  fast convergence matters.
- WebRTC spec on ICE: https://www.w3.org/TR/webrtc/#ice-candidate
- Pion WASM ICE candidate dispatch: pion/webrtc/v4/peerconnection_js.go
  around `onICECandidateHandler`.

## File map (where the work would land)

- internal/rtc/rtc.go — extend `signalMsg`, drop `waitGather` (and the
  `gatherDeadline` constant), drop the standalone `exchangeTeardown`
  function in favor of the manager's method.
- internal/rtc/trickle.go — new file holding `trickleManager`.
- internal/rtc/session.go — add a `tm` field to `Session`; thread the
  manager through `openSession` and `Close`.
- internal/rtc/rtc.go's `Send` and `Recv` — three places where the
  same trickle setup is needed; factor into the manager so the
  call-site code is `tm := newTrickleManager(...); defer tm.close();`.
