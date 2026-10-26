package bbr

import (
	"math"
	"testing"
	"time"
)

// TestConfigGuards: NaN/Inf/negative bandwidth seeds, out-of-band RTT
// seeds, and inverted segment rails must all degrade to safe defaults,
// never to NaN estimates, pinned windows, or panics.
func TestConfigGuards(t *testing.T) {
	for _, seedBW := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, 0} {
		fc := &fclock{t: time.Unix(0, 0)}
		ctl := NewController(Config{SeedBWBps: seedBW, Now: fc.now})
		if bw := ctl.MaxBW(); math.IsNaN(bw) || math.IsInf(bw, 0) || bw <= 0 {
			t.Fatalf("seedBW %v: MaxBW %v, want positive default", seedBW, bw)
		}
		if r := ctl.PacingRate(); math.IsNaN(r) || math.IsInf(r, 0) || r < minPacingRate {
			t.Fatalf("seedBW %v: pacing %v, want >= floor", seedBW, r)
		}
	}
	// RTT seed above the physical ceiling clamps into the band: a 30s
	// seed would otherwise pin the window at max for a full window.
	for _, seedRTT := range []time.Duration{-time.Second, 0, 30 * time.Second, time.Hour} {
		fc := &fclock{t: time.Unix(0, 0)}
		ctl := NewController(Config{SeedRTT: seedRTT, Now: fc.now})
		if rtt := ctl.MinRTT(); rtt <= 0 || rtt > minRTTCeil {
			t.Fatalf("seedRTT %v: MinRTT %v, want inside (0, %v]", seedRTT, rtt, minRTTCeil)
		}
	}
	// Inverted rails: the proven-static floor wins, the ceiling follows.
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(Config{MinSegs: 512, MaxSegs: 64, Now: fc.now})
	if w := ctl.WindowSegments(); w < 512 {
		t.Fatalf("inverted rails: window %d, want >= floor 512", w)
	}
	if w := ctl.SendWindowSegments(); w < 512 {
		t.Fatalf("inverted rails: send window %d, want >= floor 512", w)
	}
}

// TestStartupExitsOnBootStall: a total stall from boot (zero-rate rounds,
// seed never confirmed) must still leave Startup within a few rounds.
// Before the fix, fullBW==0 reset the no-growth counter every round and
// Startup held its 2.77x doubling gain forever, salting the queue that the
// first recovery then suffered through.
func TestStartupExitsOnBootStall(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	for i := 0; i < 30; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 0, Interval: 100 * time.Millisecond})
	}
	if ctl.State() == StateStartup {
		t.Fatal("still in startup after 30 zero-rate rounds from boot")
	}
	// Recovery afterwards must converge, not inherit a wedged gain.
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 600, 100*time.Millisecond)
	if bw := ctl.MaxBW(); math.Abs(bw-10e6/8)/(10e6/8) > 0.4 {
		t.Fatalf("post-stall bw %.0f, want ~1250000", bw)
	}
}

// TestZeroIntervalSkipped: a carried (Interval 0) tick must not divide by
// zero, must not append a point, and must not disturb the model.
func TestZeroIntervalSkipped(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}
	runTicks(fc, ctl, p, 50, 100*time.Millisecond)
	before := ctl.MaxBW()
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{Interval: 0})
	fc.step(-50 * time.Millisecond) // clock jump backwards
	ctl.Update(Sample{Interval: 0})
	if got := ctl.MaxBW(); got != before {
		t.Fatalf("zero-interval ticks moved MaxBW %.0f -> %.0f", before, got)
	}
}

// TestHugeCountersNoOverflow: lifetime byte counters near 2^63 must not
// wrap the round clock or produce negative windows.
func TestHugeCountersNoOverflow(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	ctl.delivered = 1<<63 - 1<<30
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{DeliveredBytes: 1 << 20, Interval: 100 * time.Millisecond})
	if w := ctl.WindowSegments(); w < 16 {
		t.Fatalf("window %d after huge counters, want sane", w)
	}
	if math.IsNaN(ctl.PacingRate()) || ctl.PacingRate() <= 0 {
		t.Fatalf("pacing %v after huge counters", ctl.PacingRate())
	}
}

// TestMinRTTNeverExceedsCeil: even under permanent queue inflation the
// damped-expiry doubling must stay bounded by the physical ceiling.
func TestMinRTTNeverExceedsCeil(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	for i := 0; i < 200; i++ {
		fc.step(11 * time.Second) // force expiry every tick
		ctl.Update(Sample{
			DeliveredBytes: 100000,
			Interval:       100 * time.Millisecond,
			RTT:            1900 * time.Millisecond,
			HasRTT:         true,
		})
	}
	if rtt := ctl.MinRTT(); rtt > minRTTCeil {
		t.Fatalf("MinRTT %v exceeds ceiling %v under inflation", rtt, minRTTCeil)
	}
}

// TestAllGainStatesBounded paces every controller state across extreme
// estimates and asserts finite, floored outputs.
func TestAllGainStatesBounded(t *testing.T) {
	for _, st := range []State{StateStartup, StateDrain, StateProbeBW, StateProbeRTT} {
		fc := &fclock{t: time.Unix(0, 0)}
		ctl := NewController(testConfig(fc))
		ctl.state = st
		ctl.maxBW = 1e12
		ctl.minRTT = minRTTCeil
		ctl.recompute()
		if r := ctl.PacingRate(); math.IsNaN(r) || math.IsInf(r, 0) || r <= 0 {
			t.Fatalf("state %v: pacing %v", st, r)
		}
		if w := ctl.WindowSegments(); w < 16 || w > 2048 {
			t.Fatalf("state %v: window %d outside rails", st, w)
		}
	}
}

func BenchmarkControllerUpdate(b *testing.B) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	s := Sample{DeliveredBytes: 125000, Interval: 100 * time.Millisecond, RTT: 80 * time.Millisecond, HasRTT: true}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(s)
	}
}

func BenchmarkSustainedBW(b *testing.B) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	for i := 0; i < 80; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 125000, Interval: 100 * time.Millisecond})
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ctl.SustainedBW()
	}
}
