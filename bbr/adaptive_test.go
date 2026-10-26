package bbr

import (
	"testing"
)

func feedProfiler(p *KCPProfiler, bw float64, n int) (changes int, degraded bool) {
	for i := 0; i < n; i++ {
		if c, _ := p.Update(bw); c {
			changes++
		}
		degraded = p.Degraded()
	}
	return changes, degraded
}

// TestProfilerEngagesOnCollapse: a sustained halving engages degraded
// exactly once, after the sustain run — not on the first bad tick.
func TestProfilerEngagesOnCollapse(t *testing.T) {
	p := &KCPProfiler{}
	if _, d := feedProfiler(p, 10e6/8, 50); d {
		t.Fatal("engaged on healthy link")
	}
	if c, _ := feedProfiler(p, 4e6/8, profilerEngageTicks-1); c != 0 {
		t.Fatalf("switched %d times before sustain run, want 0", c)
	}
	if p.Degraded() {
		t.Fatal("engaged before sustain run completed")
	}
	c, d := feedProfiler(p, 4e6/8, 1)
	if c != 1 || !d {
		t.Fatalf("changes=%d degraded=%v at sustain end, want 1/true", c, d)
	}
}

// TestProfilerIgnoresDips: bench gaps and momentary dips below the ratio
// must not flip the profile.
func TestProfilerIgnoresDips(t *testing.T) {
	p := &KCPProfiler{}
	feedProfiler(p, 10e6/8, 50)
	// 5s collapse then healthy: well under the ~10s sustain run.
	if c, _ := feedProfiler(p, 1e6/8, 50); c != 0 {
		t.Fatalf("dip flipped profile %d times", c)
	}
	if p.Degraded() {
		t.Fatal("short dip engaged degraded")
	}
	// Single-tick noise at zero: no signal, hold.
	if c, _ := feedProfiler(p, 0, 1); c != 0 {
		t.Fatal("zero sample switched profile")
	}
}

// TestProfilerDwellBlocksFlap: after engaging, even full recovery cannot
// switch back inside the dwell window.
func TestProfilerDwellBlocksFlap(t *testing.T) {
	p := &KCPProfiler{}
	feedProfiler(p, 10e6/8, 50)
	feedProfiler(p, 1e6/8, profilerEngageTicks)
	if !p.Degraded() {
		t.Fatal("did not engage")
	}
	// Recovery-level signal for half the dwell: must hold degraded.
	if c, _ := feedProfiler(p, 10e6/8, profilerDwellTicks/2); c != 0 {
		t.Fatalf("dwell violated: %d switches", c)
	}
	if !p.Degraded() {
		t.Fatal("left degraded inside dwell")
	}
}

// TestProfilerRecovers: sustained return above the recover ratio restores
// calm exactly once.
func TestProfilerRecovers(t *testing.T) {
	p := &KCPProfiler{}
	feedProfiler(p, 10e6/8, 50)
	feedProfiler(p, 1e6/8, profilerEngageTicks)
	feedProfiler(p, 10e6/8, profilerDwellTicks) // run out the dwell
	c, d := feedProfiler(p, 10e6/8, profilerRecoverTicks)
	if c != 1 || d {
		t.Fatalf("changes=%d degraded=%v after recovery run, want 1/false", c, d)
	}
}

// TestProfilerDecayRecalms: a permanent step-down is a new normal, not a
// transient episode: best decays toward current and calm returns without
// any recovery in the link.
func TestProfilerDecayRecalms(t *testing.T) {
	p := &KCPProfiler{}
	feedProfiler(p, 10e6/8, 50)
	feedProfiler(p, 4e6/8, profilerEngageTicks)
	if !p.Degraded() {
		t.Fatal("did not engage on step-down")
	}
	// Hold the low rate indefinitely: decay must eventually re-calm.
	switched := false
	for i := 0; i < 6000 && !switched; i++ {
		if c, d := p.Update(4e6 / 8); c && !d {
			switched = true
		}
	}
	if !switched {
		t.Fatal("permanent new normal never re-calmed")
	}
}

// TestProfilerBestRatchets: best only moves up on real signal, giving the
// ratios a stable anchor on a healthy link with jitter.
func TestProfilerBestRatchets(t *testing.T) {
	p := &KCPProfiler{}
	p.Update(10e6 / 8)
	if b := p.Best(); b != 10e6/8 {
		t.Fatalf("best %.0f, want seed peak", b)
	}
	// Jitter around the peak must neither move best down fast nor engage.
	feedProfiler(p, 9e6/8, 200)
	if p.Degraded() {
		t.Fatal("10 percent jitter engaged degraded")
	}
	p.Update(12e6 / 8)
	if b := p.Best(); b != 12e6/8 {
		t.Fatalf("best %.0f, want new peak", b)
	}
}

// TestProfilerRespectsBounds is a fuzz over random walk bandwidths:
// switches stay countable (no flap storm) and degraded always clears when
// the link returns to peak for long enough.
func TestProfilerRespectsBounds(t *testing.T) {
	p := &KCPProfiler{}
	levels := []float64{10e6 / 8, 3e6 / 8, 10e6 / 8, 1e6 / 8, 9e6 / 8, 10e6 / 8}
	for _, lv := range levels {
		feedProfiler(p, lv, 700)
	}
	if s := p.Switches(); s > 6 {
		t.Fatalf("%d switches over 6 phases, want no flap storm", s)
	}
	feedProfiler(p, 10e6/8, 2000)
	if p.Degraded() {
		t.Fatal("stuck degraded after long return to peak")
	}
}
