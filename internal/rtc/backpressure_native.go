//go:build !js

package rtc

import (
	"io"

	"github.com/pion/webrtc/v4"
)

// wrapSendWriter is a no-op on native pion: the SCTP layer already applies
// flow control inside Write, so the payload pump blocks naturally rather
// than throwing on overflow the way the browser's RTCDataChannel.send does.
func wrapSendWriter(_ *webrtc.DataChannel, w io.Writer) io.Writer { return w }
