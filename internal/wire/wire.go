// Package wire defines the JSON control frames exchanged between conduit
// clients and the signaling server during rendezvous.
//
// Client → server:
//
//	{"op":"reserve"}              sender reserves a slot
//	{"op":"join","slot":N}        receiver joins slot N
//
// Server → client:
//
//	{"op":"reserved","slot":N}    ack of reservation; sender should display N
//	{"op":"paired"}               both peers attached; opaque relay begins
//	{"op":"error","code":...}     protocol error; server closes the socket
//
// After receiving a "paired" frame on a socket, all subsequent frames on that
// socket are opaque bytes relayed verbatim between the two peers. The server
// does not parse them.
package wire

const ProtocolVersion = "conduit/v1"

const (
	OpReserve  = "reserve"
	OpJoin     = "join"
	OpReserved = "reserved"
	OpPaired   = "paired"
	OpError    = "error"
)

// Error codes returned in Error.Code.
const (
	ErrBadRequest   = "bad_request"
	ErrRateLimited  = "rate_limited"
	ErrSlotNotFound = "slot_not_found"
	ErrExpired      = "expired"
	ErrInternal     = "internal"
)

// ClientHello is the first frame a client sends after WebSocket accept.
// Slot is only used with Op == OpJoin.
type ClientHello struct {
	Op   string `json:"op"`
	Slot uint32 `json:"slot,omitempty"`
}

type Reserved struct {
	Op   string `json:"op"`
	Slot uint32 `json:"slot"`
}

type Paired struct {
	Op string `json:"op"`
}

type Error struct {
	Op      string `json:"op"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Envelope carries any control frame for decoding when the op is not yet known.
type Envelope struct {
	Op      string `json:"op"`
	Slot    uint32 `json:"slot,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
