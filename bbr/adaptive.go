package bbr

// KCP profile adaptation: calm by default, aggressive retransmission only
// while the path is demonstrably degraded.
//
// Substrate constraint (load-bearing): this KCP runs over DNS-over-TCP, a
// reliable ordered substrate that already retransmits underneath. Timer
// aggression (nodelay=1 drops the RTO floor 100ms to 30ms) would race TCP
// timers and manufacture spurious duplicate retransmits on jittery paths
// (cf. RFC 5682/4138 on spurious RTOs, RFC 2988 on conservative minimum
// RTO). So the degraded profile keeps timers calm and adds only
// evidence-triggered recovery: duplicate-ACK fast retransmit (fires solely
// when later segments demonstrably got through) plus immediate ACK flush
// (shrinks KCP-level RTT inflation; the extra upstream is small packets
// that bypass the query pacer). Classic kcptun fast presets are rejected
// for this transport; they assume raw UDP where KCP is the only recovery.
//
// The detector keys off the controller's own maxBW with hysteresis plus
// dwell, so bench gaps and momentary dips cannot flap it. Session-best
// decays slowly toward current, so a permanent new normal re-calms
// instead of wedging aggressive forever: fast recovery is for transient
// loss episodes, not for a permanently slower link (whose window the
// controller has already adapted).
const (
	kcpCalmNoDelay  = 0
	kcpCalmInterval = 10
	kcpCalmResend   = 0
	kcpCalmNC       = 1

	kcpDegradedResend = 2

	// profilerEngageRatio trips degraded when maxBW sits below this
	// fraction of session-best.
	profilerEngageRatio = 0.5
	// profilerRecoverRatio restores calm above this fraction of best.
	profilerRecoverRatio = 0.8
	// profilerEngageTicks is how long (in 100ms ticks) the collapse must
	// sustain before engaging: ~10s, past the 8s estimator horizon so a
	// single aged-out window cannot flip it.
	profilerEngageTicks = 100
	// profilerRecoverTicks guards recovery symmetrically (~15s).
	profilerRecoverTicks = 150
	// profilerDwellTicks freezes the detector after any switch (~30s):
	// no flap, both directions.
	profilerDwellTicks = 300
	// profilerBestDecay eases session-best toward current each tick, so a
	// permanent step-down re-calms within minutes (half-life ~69s).
	profilerBestDecay = 0.999
)

// KCPProfiler decides between the calm and degraded KCP profiles from the
// controller's bandwidth estimate. Zero value is ready: calm, no history.
// Not safe for concurrent use; call from the single BBR ticker goroutine.
type KCPProfiler struct {
	degraded     bool
	best         float64
	lowTicks     int
	highTicks    int
	dwell        int
	engagedTicks int // observability: ticks spent degraded
	switches     int // observability: profile changes so far
}

// Update folds one bandwidth estimate (B/s) into the detector. It reports
// whether the profile changed on this call and which profile now holds.
// Non-positive estimates carry no signal: the detector holds.
func (p *KCPProfiler) Update(maxBW float64) (changed, degraded bool) {
	if maxBW <= 0 {
		return false, p.degraded
	}
	if maxBW > p.best {
		p.best = maxBW
	} else if p.best > maxBW {
		if d := p.best * profilerBestDecay; d > maxBW {
			p.best = d
		} else {
			p.best = maxBW
		}
	}
	if p.dwell > 0 {
		p.dwell--
		if p.degraded {
			p.engagedTicks++
		}
		return false, p.degraded
	}
	if !p.degraded {
		if maxBW < profilerEngageRatio*p.best {
			p.lowTicks++
		} else {
			p.lowTicks = 0
		}
		if p.lowTicks >= profilerEngageTicks {
			p.degraded = true
			p.lowTicks = 0
			p.dwell = profilerDwellTicks
			p.switches++
			p.engagedTicks++
			return true, true
		}
		return false, false
	}
	if p.degraded {
		p.engagedTicks++
	}
	if maxBW > profilerRecoverRatio*p.best {
		p.highTicks++
	} else {
		p.highTicks = 0
	}
	if p.highTicks >= profilerRecoverTicks {
		p.degraded = false
		p.highTicks = 0
		p.dwell = profilerDwellTicks
		p.switches++
		return true, false
	}
	return false, true
}

// Degraded reports the current profile without feeding a sample.
func (p *KCPProfiler) Degraded() bool { return p.degraded }

// Best returns the session-best estimate the detector ratios against.
func (p *KCPProfiler) Best() float64 { return p.best }

// Switches returns how many profile changes have happened (logs/tests).
func (p *KCPProfiler) Switches() int { return p.switches }
