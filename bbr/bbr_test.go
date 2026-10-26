package bbr

import (
	"math"
	"sync"
	"testing"
	"time"
)

// fclock is a manual clock for deterministic tests.
type fclock struct{ t time.Time }

func (f *fclock) now() time.Time { return f.t }
func (f *fclock) step(d time.Duration) {
	f.t = f.t.Add(d)
}

func testConfig(fc *fclock) Config {
	return Config{
		SeedBWBps: 250000, // 2 Mbps seed
		SeedRTT:   100 * time.Millisecond,
		MinSegs:   16,
		MaxSegs:   2048,
		SegBytes:  500,
		Now:       fc.now,
	}
}

// plant simulates a link: delivers up to linkBps, always backlogged
// (never app-limited), fixed RTT.
type plant struct {
	linkBps float64
	rtt     time.Duration
}

func (p *plant) sample(fc *fclock, ctl *Controller, dt time.Duration) {
	// What the controller asks for vs what the link gives.
	demand := ctl.PacingRate() * dt.Seconds()
	got := math.Min(demand, p.linkBps*dt.Seconds())
	fc.step(dt)
	ctl.Update(Sample{
		DeliveredBytes: uint64(got),
		Interval:       dt,
		AppLimited:     false,
		RTT:            p.rtt,
		HasRTT:         true,
	})
}

func runTicks(fc *fclock, ctl *Controller, p *plant, n int, dt time.Duration) {
	for i := 0; i < n; i++ {
		p.sample(fc, ctl, dt)
	}
}

// TestStartupConverges: on a 10 Mbps / 50 ms link the controller must leave
// startup, pass drain, and settle the window near BDP (125 segs).
func TestStartupConverges(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 400, 100*time.Millisecond) // 40 simulated seconds
	if ctl.State() == StateStartup {
		t.Fatal("still in startup after 40s on a fixed link")
	}
	// BDP = 1.25MB/s * 0.05s = 62.5KB = 125 segs; allow estimator slack.
	if w := ctl.WindowSegments(); w < 80 || w > 400 {
		t.Fatalf("window %d segs, want near BDP 125", w)
	}
	if bw := ctl.MaxBW(); math.Abs(bw-10e6/8)/(10e6/8) > 0.3 {
		t.Fatalf("bw estimate %.0f B/s, want ~1250000", bw)
	}
	if rtt := ctl.MinRTT(); rtt != 50*time.Millisecond {
		t.Fatalf("rtt %v, want 50ms", rtt)
	}
}

// TestWrongSeedsConverge: seeds an order of magnitude off in both
// directions must still converge by measurement (defaults are priors,
// never destiny).
func TestWrongSeedsConverge(t *testing.T) {
	for _, seed := range []float64{100e6 / 8, 100e3 / 8} {
		fc := &fclock{t: time.Unix(0, 0)}
		cfg := testConfig(fc)
		cfg.SeedBWBps = seed
		ctl := NewController(cfg)
		p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
		runTicks(fc, ctl, p, 1200, 100*time.Millisecond) // 120s: probing ratchets up
		if bw := ctl.MaxBW(); math.Abs(bw-10e6/8)/(10e6/8) > 0.4 {
			t.Fatalf("seed %.0f: bw %.0f, want ~1250000", seed, bw)
		}
	}
}

// TestAppLimitedPreserved: idle trickle must not drag the estimate down;
// resuming bulk restores full rate immediately from the kept estimate.
func TestAppLimitedPreserved(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 200, 100*time.Millisecond)
	before := ctl.MaxBW()
	// Idle: 64 B/s trickle, app-limited, for 30 simulated seconds.
	for i := 0; i < 300; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 8, Interval: 100 * time.Millisecond, AppLimited: true})
	}
	if after := ctl.MaxBW(); math.Abs(after-before)/before > 0.05 {
		t.Fatalf("idle moved estimate %.0f -> %.0f", before, after)
	}
	// Resume: pacing must be alive and near the kept estimate (gain varies
	// legitimately with the probe cycle, so band it, don't pin it).
	if r := ctl.PacingRate(); r < before*0.5 || r > before*1.5 {
		t.Fatalf("pacing %.0f after idle, want near %.0f", r, before)
	}
}

// TestPolicerFreeze: a hard-capped link is just a lower BtlBw: the windowed
// max converges there without any separate policer model. The old
// plateau-duration latch mistook self-inflicted starvation for a policer
// and pinned pacing at 1KB/s (live 0 B/s bulk); it is removed, and
// Policed() stays false by design.
func TestPolicerFreeze(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 5e6 / 8, rtt: 50 * time.Millisecond} // hard cap
	runTicks(fc, ctl, p, 1500, 100*time.Millisecond)
	if ctl.Policed() {
		t.Fatal("policer latch engaged; removed by design")
	}
	if bw := ctl.MaxBW(); bw > 5e6/8*1.5 || bw < 5e6/8*0.5 {
		t.Fatalf("estimate %.0f, want near policed cap %.0f", bw, 5e6/8)
	}
}

// TestProbeRTTFires: a stale min-RTT with continuous sending must trigger a
// drain episode, then resume probing.
func TestProbeRTTFires(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 80 * time.Millisecond}
	runTicks(fc, ctl, p, 100, 100*time.Millisecond)
	// Freeze RTT samples above the floor: no new lows for >10s of sending.
	// ProbeRTT is transient (~200ms), so record whether it is ever entered
	// rather than asserting the end state.
	seenRTT := false
	for i := 0; i < 200; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 125000, Interval: 100 * time.Millisecond})
		if ctl.State() == StateProbeRTT {
			seenRTT = true
		}
	}
	if !seenRTT {
		t.Fatal("probertt never entered after stale min-RTT")
	}
	// And it must exit again on its own.
	for i := 0; i < 10 && ctl.State() == StateProbeRTT; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 1000, Interval: 100 * time.Millisecond})
	}
	if ctl.State() == StateProbeRTT {
		t.Fatal("stuck in probertt past its duration")
	}
}

// TestClamps: absurd links hit the rails, never escape them.
func TestClamps(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	// Gigabit link, tiny RTT: BDP wants ~25000 segs.
	runTicks(fc, ctl, &plant{linkBps: 1e9 / 8, rtt: 10 * time.Millisecond}, 600, 100*time.Millisecond)
	if w := ctl.WindowSegments(); w != 2048 {
		t.Fatalf("window %d, want ceiling 2048", w)
	}
	// Dial-up: BDP wants ~2 segs.
	fc2 := &fclock{t: time.Unix(0, 0)}
	ctl2 := NewController(testConfig(fc2))
	runTicks(fc2, ctl2, &plant{linkBps: 56e3 / 8, rtt: 300 * time.Millisecond}, 600, 100*time.Millisecond)
	if w := ctl2.WindowSegments(); w != 16 {
		t.Fatalf("window %d, want floor 16", w)
	}
}

// TestHorizonFloor: with a collapsed min-RTT the memory horizon must still
// span the wall-time floor, so a momentary gap cannot amnesia the estimate
// (live failure: 829us min-RTT shrank memory to ~8ms).
func TestHorizonFloor(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 100, 100*time.Millisecond)
	before := ctl.MaxBW()
	// Collapse min-RTT below any physical value, then go quiet briefly
	// (less than the 2s floor but far past 10 rounds of the fake RTT).
	ctl.minRTT = 100 * time.Microsecond
	for i := 0; i < 5; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{Interval: 100 * time.Millisecond, AppLimited: true})
	}
	if after := ctl.MaxBW(); math.Abs(after-before)/before > 0.05 {
		t.Fatalf("short gap moved estimate %.0f -> %.0f", before, after)
	}
}

// TestTracksDownward: a sustained degradation must pull the estimate down
// (windowed max expiry), not stick at history forever.
func TestTracksDownward(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	runTicks(fc, ctl, &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}, 100, 100*time.Millisecond)
	if bw := ctl.MaxBW(); bw < 10e6/8*0.7 {
		t.Fatalf("setup estimate %.0f too low", bw)
	}
	runTicks(fc, ctl, &plant{linkBps: 1e6 / 8, rtt: 50 * time.Millisecond}, 300, 100*time.Millisecond)
	if bw := ctl.MaxBW(); bw > 1e6/8*2 {
		t.Fatalf("estimate %.0f stuck above degraded link %.0f", bw, 1e6/8)
	}
}

// TestPacerMaxWait: no single send waits past the cap no matter how far
// the estimate collapses (the anti-deadlock bound).
func TestPacerMaxWait(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	p := NewPacer(fc.now)
	p.SetRate(10, 1500) // 10 B/s trickle: 1MB would take days uncapped
	if d := p.Advance(1 << 20); d > maxPacerWait {
		t.Fatalf("pacer wait %v exceeds cap %v", d, maxPacerWait)
	}
	// And the bucket floor keeps subsequent sends bounded too.
	if d := p.Advance(1 << 20); d > maxPacerWait {
		t.Fatalf("second wait %v exceeds cap %v", d, maxPacerWait)
	}
}

// TestPacerConcurrent hammers Advance (send path) and SetRate (ticker
// path) from many goroutines: production shares one Pacer across exactly
// those two goroutines with no external lock, so any race here is a live
// data race on pacing decisions. Run with -race.
func TestPacerConcurrent(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	p := NewPacer(fc.now)
	p.SetRate(1e6/8, 8000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				p.Advance(500 + g)
				if i%50 == 0 {
					p.SetRate(1e6/8+float64(g*1000), 8000)
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestExitStartupEarly: the degraded verdict ends Startup immediately with
// drain gain, and is a no-op in every other state.
func TestExitStartupEarly(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	if ctl.State() != StateStartup {
		t.Fatalf("new controller in %v, want startup", ctl.State())
	}
	ctl.ExitStartupEarly()
	if ctl.State() != StateDrain {
		t.Fatalf("after early exit in %v, want drain", ctl.State())
	}
	if ctl.gain != drainGain {
		t.Fatalf("gain %v, want drain %v", ctl.gain, drainGain)
	}
	// Drain must still exit on its normal condition (queue drained).
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{DeliveredBytes: 1000, Interval: 100 * time.Millisecond})
	// No-op outside Startup: reach ProbeBW, then call again.
	runTicks(fc, ctl, &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}, 400, 100*time.Millisecond)
	if ctl.State() != StateProbeBW {
		t.Fatalf("setup ended in %v, want probebw", ctl.State())
	}
	ctl.ExitStartupEarly()
	if ctl.State() != StateProbeBW {
		t.Fatalf("early exit outside startup moved state to %v", ctl.State())
	}
}

// TestStartupEarlyExitPreservesConvergence: bailing to Drain on a false
// degraded signal must not break eventual convergence (Drain exits to
// ProbeBW and probing ratchets back up).
func TestStartupEarlyExitPreservesConvergence(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 20, 100*time.Millisecond)
	ctl.ExitStartupEarly()
	runTicks(fc, ctl, p, 600, 100*time.Millisecond)
	if bw := ctl.MaxBW(); math.Abs(bw-10e6/8)/(10e6/8) > 0.3 {
		t.Fatalf("bw %.0f after early exit, want ~1250000", bw)
	}
}

// TestGainCyclePattern: steady-state gains must walk up, down, cruise.
func TestGainCyclePattern(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 200, 100*time.Millisecond) // settle to probebw
	seen := map[float64]bool{}
	for i := 0; i < 40; i++ {
		fc.step(100 * time.Millisecond)
		// Fresh sample each round to advance the cycle.
		ctl.Update(Sample{DeliveredBytes: 125000, Interval: 100 * time.Millisecond})
		seen[ctl.gain] = true
	}
	for _, g := range []float64{probeUpGain, probeDownGain, cruiseGain} {
		if !seen[g] {
			t.Fatalf("gain %v never observed in cycle", g)
		}
	}
}

// TestPacerMath: primed burst sends immediately, refill accrues over time,
// overdraft sleeps the deficit, zero rate never blocks.
func TestPacerMath(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	p := NewPacer(fc.now)
	p.SetRate(1e6/8, 8000) // 1 Mbps (125000 B/s), 8KB burst, primed full
	if d := p.Advance(8000); d != 0 {
		t.Fatalf("primed burst blocked %v, want immediate", d)
	}
	// Refill 80ms at 125000 B/s = 10000B, capped to the 8000B burst.
	fc.step(80 * time.Millisecond)
	if d := p.Advance(8000); d != 0 {
		t.Fatalf("refilled bucket blocked %v", d)
	}
	// Overdraft 16000B at 125000 B/s => 128ms owed.
	if d := p.Advance(16000); d < 127*time.Millisecond || d > 129*time.Millisecond {
		t.Fatalf("deficit wait %v, want ~128ms", d)
	}
	// Zero rate never blocks.
	p.SetRate(0, 0)
	if d := p.Advance(1 << 30); d != 0 {
		t.Fatalf("zero-rate pacer blocked %v", d)
	}
}
