//go:build !js

package rtc

import (
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// applyNativeSettings tunes SettingEngine knobs that pion only exposes on
// native builds. The JS build (pion/webrtc's RTCPeerConnection wrapper) hands
// these responsibilities to the browser, which has its own policies; the
// corresponding setters do not exist there.
func applyNativeSettings(se *webrtc.SettingEngine) {
	// Disable mDNS ICE candidate obfuscation. The default mode advertises
	// host candidates as random .local names resolvable only via an mDNS
	// responder, which breaks loopback connectivity in test environments
	// and constrained networks.
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	// Without loopback candidates, same-host peers only see LAN IPs; under
	// load ICE can sit behind srflx/prflx minimum-wait timers and miss tight
	// CLI deadlines. Loopback host candidates make 127.0.0.1/::1 pairs
	// available immediately for local transfers.
	se.SetIncludeLoopbackCandidate(true)
	// Defaults wait 500ms/1s before nominating srflx/prflx pairs; that often
	// consumes the tail of short contexts even when a host pair is viable.
	se.SetSrflxAcceptanceMinWait(0)
	se.SetPrflxAcceptanceMinWait(0)
	// Default 5s STUN gather timeout can dominate small operation budgets when
	// srflx candidates are requested alongside sluggish STUN.
	se.SetSTUNGatherTimeout(time.Second)
}
