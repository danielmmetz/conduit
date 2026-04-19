//go:build js && wasm

package rtc

import "github.com/pion/webrtc/v4"

// applyNativeSettings is a no-op under js/wasm: pion's JS wrapper hands ICE
// tuning to the browser's RTCPeerConnection and does not expose the
// corresponding setters.
func applyNativeSettings(*webrtc.SettingEngine) {}
