//go:build !js

package rtc

import "github.com/pion/webrtc/v4"

// detectRoute reads the ICE selected candidate pair and classifies the
// transport. Returns RouteUnknown if the chain is not yet populated or the
// selected pair is unavailable — pion exposes the chain on both native and
// js/wasm builds, but the selected pair only becomes non-nil after ICE
// concludes. Callers should sample after the data channel has opened.
func detectRoute(pc *webrtc.PeerConnection) Route {
	sctp := pc.SCTP()
	if sctp == nil {
		return RouteUnknown
	}
	dtls := sctp.Transport()
	if dtls == nil {
		return RouteUnknown
	}
	ice := dtls.ICETransport()
	if ice == nil {
		return RouteUnknown
	}
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil || pair.Local == nil || pair.Remote == nil {
		return RouteUnknown
	}
	if pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay {
		return RouteRelayed
	}
	return RouteDirect
}
