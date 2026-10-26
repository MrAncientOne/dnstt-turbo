package cake

import (
	"testing"
	"time"
)

// TestRateParamsShape checks cake_set_rate field behavior: quantum clamps,
// unlimited yields zero shaper clocks with a sane quantum, and target grows
// with falling rate (serialisation delay dominates).
func TestRateParamsShape(t *testing.T) {
	fast := ComputeRateParams(10_000_000, 1400, DefaultTargetNs, DefaultRTTEstNs)
	if fast.FlowQuantum != 1514 {
		t.Fatalf("10MB/s quantum=%d, want clamped 1514", fast.FlowQuantum)
	}
	if fast.RateNs == 0 || fast.RateShft == 0 {
		t.Fatal("shaped rate must produce fixed-point clocks")
	}
	slow := ComputeRateParams(64_000, 1400, DefaultTargetNs, DefaultRTTEstNs)
	if slow.FlowQuantum != 300 {
		t.Fatalf("64Kb/s quantum=%d, want floored 300", slow.FlowQuantum)
	}
	if slow.TargetNs <= fast.TargetNs {
		t.Fatalf("slow target=%d <= fast target=%d, want growth", slow.TargetNs, fast.TargetNs)
	}
	unlim := ComputeRateParams(0, 1400, DefaultTargetNs, DefaultRTTEstNs)
	if unlim.FlowQuantum != 1514 || unlim.RateNs != 0 {
		t.Fatalf("unlimited=%+v, want quantum 1514, zero clocks", unlim)
	}
	if got := BufferLimit(0, 0); got != 4<<20 {
		t.Fatalf("unlimited buffer=%d, want 4MiB floor", got)
	}
	if got := BufferLimit(10_000_000, 100_000_000); got != 4<<20 {
		// 10MB/s * 100ms / 4 = 250KB < floor.
		t.Fatalf("small buffer=%d, want 4MiB floor", got)
	}
	if got := BufferLimit(1_000_000_000, 100_000_000); got != 25_000_000 {
		t.Fatalf("1GB/s buffer=%d, want 25MB", got)
	}
}

// TestShaperPaces verifies deficit-mode shaping: back-to-back chunks of the
// same size advance the gate by one serialisation delay each, the failsafe
// runs 1.5x, and an idle gap resets the clocks.
func TestShaperPaces(t *testing.T) {
	rp := ComputeRateParams(1_000_000, 1400, DefaultTargetNs, DefaultRTTEstNs)
	s := Shaper{RateNs: rp.RateNs, RateShft: rp.RateShft}
	t0 := time.Now()
	s.IdleReset(t0)
	if w := s.Wait(t0); w != 0 {
		t.Fatalf("fresh shaper waits %v, want 0", w)
	}
	const n = uint32(1400)
	s.Advance(n, t0, false)
	per := time.Duration((uint64(n) * rp.RateNs) >> rp.RateShft)
	if w := s.Wait(t0); w <= 0 || w > 2*per+time.Millisecond {
		t.Fatalf("wait=%v after one chunk, want ~(1..2)x serialisation %v", w, per)
	}
	// Failsafe is the binding gate only if it lies nearer; deliver must
	// be possible once the primary clock passes even if asked early.
	t1 := t0.Add(3 * per)
	if w := s.Wait(t1); w != 0 {
		t.Fatalf("wait=%v long after, want 0", w)
	}
	// Idle reset clears stale clocks.
	s.IdleReset(t1.Add(time.Second))
	if w := s.Wait(t1.Add(time.Second)); w != 0 {
		t.Fatalf("wait=%v after idle reset, want 0", w)
	}
	// Disabled shaper never waits and never advances.
	off := Shaper{}
	off.Advance(n, t0, false)
	if w := off.Wait(t0); w != 0 {
		t.Fatalf("unlimited shaper waits %v, want 0", w)
	}
}

// TestCobaltConverges pins the invsqrt fast path: count 0/1 map near 1.0,
// and Newton stepping past the cache stays within (0, 1.0].
func TestCobaltConverges(t *testing.T) {
	var v CobaltVars
	cobaltInvsqrt(&v, 0)
	if v.RecInvSqrt != 0xFFFFFFFF {
		t.Fatalf("count0 rec=%d, want ~1.0", v.RecInvSqrt)
	}
	for _, c := range []uint32{2, 5, 15, 16, 100, 100000} {
		cobaltInvsqrt(&v, c)
		if v.RecInvSqrt == 0 || v.RecInvSqrt > 0xFFFFFFFF {
			t.Fatalf("count%d rec=%d out of range", c, v.RecInvSqrt)
		}
	}
}

// TestCobaltDropFiresOnPersistentQueue: a chunk stuck past target+interval
// with an armed drop_next must signal drop (the AQM path the tunnel keeps
// charged for deeper queues).
func TestCobaltDropFiresOnPersistentQueue(t *testing.T) {
	p := &CobaltParams{Interval: int64(100 * time.Millisecond), Target: int64(5 * time.Millisecond), MTUTime: int64(time.Millisecond), PInc: 1 << 24, PDec: 1 << 20}
	var v CobaltVars
	now := time.Now()
	old := now.Add(-time.Second)
	// Prime: first observation arms dropping with drop_next = now+interval.
	if d, _ := CobaltShouldDrop(&v, p, now, old, 1, ^uint32(0)); d != CobaltDeliver {
		t.Fatal("first over-target observation must arm, not drop")
	}
	if !v.Dropping {
		t.Fatal("dropping flag not armed")
	}
	// Past drop_next with the queue still old: drop fires.
	later := now.Add(time.Duration(p.Interval) + time.Millisecond)
	if d, _ := CobaltShouldDrop(&v, p, later, old, 1, ^uint32(0)); d != CobaltDrop {
		t.Fatal("persistent queue past drop_next must drop")
	}
	// Fresh chunk drains the signal without dropping.
	if d, _ := CobaltShouldDrop(&v, p, later, later, 1, ^uint32(0)); d != CobaltDeliver {
		t.Fatal("fresh chunk must deliver and clear dropping")
	}
	if v.Dropping {
		t.Fatal("dropping flag must clear on fresh chunk")
	}
}
