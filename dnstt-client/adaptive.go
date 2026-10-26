package main

import (
	"fmt"
	"math"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
)

var (
	bbrMinWindow = 32
	bbrMaxWindow = bbr.MaxWindowSegments
	bbrMaxFlight = 16 << 20
	bbrMinRate   = 1024.0
)

func validateAdaptiveSettings() error {
	if kcpWindow < 1 || kcpWindow > bbr.MaxWindowSegments {
		return fmt.Errorf("kcp-window must be 1..%d", bbr.MaxWindowSegments)
	}
	if bbrMinWindow < 1 || bbrMaxWindow < bbrMinWindow || bbrMaxWindow > bbr.MaxWindowSegments {
		return fmt.Errorf("require 1 <= bbr-min-window <= bbr-max-window <= %d", bbr.MaxWindowSegments)
	}
	if bbrMaxFlight < bbrMinWindow*1500 || bbrMaxFlight > 1<<30 {
		return fmt.Errorf("bbr-max-flight must cover the minimum window at 1500 bytes/segment and be <= 1 GiB")
	}
	if bbrMinRate <= 0 || math.IsNaN(bbrMinRate) || math.IsInf(bbrMinRate, 0) {
		return fmt.Errorf("bbr-min-rate must be finite and positive")
	}
	return nil
}

// Each model has its own delivery feedback, rate and window. No downstream
// measurement is used to size the upload pacer or fair-upload shaper.
type adaptiveControl struct {
	up, down      *bbr.Controller
	uploadSegment int
	downSegment   int
	carrierOnly   bool
}
type adaptiveDecision struct {
	sendWindow, receiveWindow, polls int
	pacingRate, shaperRate, burst    float64
}

func newAdaptiveControl(mtu int, now func() time.Time) *adaptiveControl {
	size := mtu - bbr.KCPHeaderSize
	if size < 1 {
		size = 1
	}
	// Both peers can use KCP's default 100ms flush interval. On short-RTT
	// paths, RTT-only windows throttle delivery before the next flush.
	cfg := bbr.Config{Now: now, MinSegs: bbrMinWindow, MaxSegs: bbrMaxWindow, MaxFlightBytes: uint64(bbrMaxFlight), WindowGain: 2, MinFlightTime: 100 * time.Millisecond, MinPacingBps: bbrMinRate, SegBytes: size, UpSegBytes: size}
	up := bbr.NewController(cfg)
	cfg.SegBytes = 1400 - bbr.KCPHeaderSize
	cfg.UpSegBytes = cfg.SegBytes
	return &adaptiveControl{up: up, down: bbr.NewController(cfg), uploadSegment: size, downSegment: cfg.SegBytes}
}

func (a *adaptiveControl) update(up, down bbr.Sample, segment, response int) adaptiveDecision {
	// Feedback reports the largest observed PUSH payload, excluding ACKs.
	// Small supported MTUs need the same byte-to-segment conversion as large
	// ones. The estimate grows as larger data segments arrive.
	if segment > 0 {
		a.down.SetSegmentBytes(segment)
		a.downSegment = segment
	}
	a.up.Update(up)
	a.down.Update(down)
	rate := a.up.PacingRate()
	// At low rates the scheduler admission clock prevents large smux/KCP
	// bursts. At high measured rates its per-chunk sub-millisecond timer
	// waits cost throughput despite the carrier already pacing payload.
	// Switch from delivery evidence, with hysteresis; never infer capacity
	// from the startup seed or the other direction's bandwidth.
	threshold := float64(max(fairChunk, 1)) / time.Millisecond.Seconds()
	sustained := a.up.SustainedBW()
	if up.DeliveredBytes > 0 && !up.AppLimited && sustained >= threshold {
		a.carrierOnly = true
	} else if sustained < threshold/2 {
		a.carrierOnly = false
	}
	shape := rate
	if a.carrierOnly || (up.AppLimited && up.DeliveredBytes == 0 && up.SentBytes == 0) {
		shape = 0
	}
	// Small bursts on slow paths; enough amortization on fast paths. Units
	// match acknowledged KCP payload, not DNS headers or downstream ACKs.
	burst := math.Max(float64(a.uploadSegment), math.Min(16384, rate*a.up.MinRTT().Seconds()/4))
	// Poll service must cover the actual receive flight, including batching
	// headroom. Window rails already enforce the byte/segment budgets.
	flight := uint64(a.down.WindowSegments()) * uint64(a.downSegment)
	return adaptiveDecision{a.up.SendWindowSegments(), a.down.WindowSegments(), PollDepthForBDP(flight, response), rate, shape, burst}
}
