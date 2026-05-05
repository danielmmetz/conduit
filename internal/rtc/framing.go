package rtc

// The data channel framing prefixes each outbound message with a one-byte
// tag so the receiver can detect end-of-stream without relying on the
// underlying transport's close semantics. SCTP stream reset (pion native)
// propagates as io.EOF on the detached reader, but the JS RTCDataChannel
// exposes no per-direction close; wrapping both sides with a tag byte gives
// uniform behavior across platforms.
//
// Frames travel both directions: sender → receiver carries tagData /
// tagEOF (payload ciphertext); receiver → sender carries tagAck (progress
// reports). Because only one peer writes each tag class, the two directions
// do not collide on a shared data channel.
const (
	tagData byte = 0 // sender → receiver: the remainder is payload ciphertext
	tagEOF  byte = 1 // sender → receiver: no more payload frames will follow
	tagAck  byte = 2 // receiver → sender: cumulative plaintext bytes received
)

// maxFrameSize bounds the receive buffer for a single DC message. Age's
// STREAM chunks are 64 KiB plus ~16 bytes of overhead; a tag byte and a bit
// of slack keep this well under the 256 KiB default browser max-message-size.
const maxFrameSize = 128 * 1024

// ackFrameSize is the on-wire size of a tagAck frame: one tag byte + int64
// big-endian total. Fits in a single data-channel message.
const ackFrameSize = 1 + 8

// defaultAckThreshold bounds ack frequency. At 256 KiB/ack and a 64 KiB age
// chunk size, one ack per ~4 payload chunks — enough resolution for a
// progress bar without flooding the reverse channel on fast local transfers.
const defaultAckThreshold = 256 * 1024
