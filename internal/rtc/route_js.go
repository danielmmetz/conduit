//go:build js && wasm

package rtc

import (
	"syscall/js"
	"time"

	"github.com/pion/webrtc/v4"
)

// detectRoute classifies the selected ICE pair via the W3C-standardized
// RTCPeerConnection.getStats(). The native build's
// ICETransport.GetSelectedCandidatePair is implemented in Chromium and
// Safari but absent in Firefox (calling it there raises a syscall/js
// panic), so the JS build avoids that path entirely.
//
// The selected pair is the candidate-pair stat with state="succeeded" and
// nominated=true; its localCandidateId / remoteCandidateId resolve to
// candidate stats whose candidateType is one of host / srflx / prflx /
// relay. We surface RouteRelayed if either side is "relay", RouteDirect
// otherwise. Returns RouteUnknown if the Promise doesn't resolve within a
// short budget — route is informational and the session has already
// opened by the time this runs.
func detectRoute(pc *webrtc.PeerConnection) Route {
	const timeout = time.Second

	done := make(chan js.Value, 1)
	resolve := js.FuncOf(func(_ js.Value, args []js.Value) any {
		var v js.Value
		if len(args) > 0 {
			v = args[0]
		}
		select {
		case done <- v:
		default:
		}
		return nil
	})
	reject := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		select {
		case done <- js.Undefined():
		default:
		}
		return nil
	})

	pc.JSValue().Call("getStats").Call("then", resolve, reject)

	var report js.Value
	select {
	case report = <-done:
		resolve.Release()
		reject.Release()
	case <-time.After(timeout):
		// Leak the js.Funcs: releasing now races with a late Promise
		// settlement that would invoke them. One-shot per timeout.
		return RouteUnknown
	}
	if report.IsUndefined() || report.IsNull() {
		return RouteUnknown
	}

	var localID, remoteID string
	pairFn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if localID != "" || len(args) < 1 {
			return nil
		}
		stat := args[0]
		if stat.Get("type").String() != "candidate-pair" {
			return nil
		}
		if !boolField(stat, "nominated") {
			return nil
		}
		if stat.Get("state").String() != "succeeded" {
			return nil
		}
		localID = stat.Get("localCandidateId").String()
		remoteID = stat.Get("remoteCandidateId").String()
		return nil
	})
	report.Call("forEach", pairFn)
	pairFn.Release()

	if localID == "" || remoteID == "" {
		return RouteUnknown
	}

	var localType, remoteType string
	candFn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		switch args[1].String() {
		case localID:
			localType = args[0].Get("candidateType").String()
		case remoteID:
			remoteType = args[0].Get("candidateType").String()
		}
		return nil
	})
	report.Call("forEach", candFn)
	candFn.Release()

	if localType == "relay" || remoteType == "relay" {
		return RouteRelayed
	}
	if localType == "" && remoteType == "" {
		return RouteUnknown
	}
	return RouteDirect
}

// boolField reads a boolean field that some browsers omit when false. A
// missing field reports as undefined; coercing to Bool would panic.
func boolField(v js.Value, name string) bool {
	f := v.Get(name)
	if f.Type() != js.TypeBoolean {
		return false
	}
	return f.Bool()
}
