package main

import (
	"testing"
)

// TestPollDepthForBDP pins the pipeline sizer: one BDP of responses plus
// headroom, floored at the static baseline, capped at the spam bound,
// unmeasured means baseline.
func TestPollDepthForBDP(t *testing.T) {
	base := pollLimit
	if got := PollDepthForBDP(71*1024, 16384); got != base {
		// 71KB BDP / 16KB responses = 4 + 4 headroom = 8 < 24 baseline.
		t.Fatalf("small BDP depth=%d, want baseline %d", got, base)
	}
	if got := PollDepthForBDP(1250*1024, 16384); got != 1250*1024/16384+4 {
		t.Fatalf("1.25MB BDP depth=%d, want 82", got)
	}
	if got := PollDepthForBDP(0, 16384); got != base {
		t.Fatalf("zero BDP depth=%d, want baseline %d", got, base)
	}
	if got := PollDepthForBDP(71*1024, 0); got != base {
		t.Fatalf("unmeasured responses depth=%d, want baseline %d", got, base)
	}
	if got := PollDepthForBDP(1<<40, 100); got != maxPollTarget {
		t.Fatalf("huge BDP depth=%d, want cap %d", got, maxPollTarget)
	}
}

// TestSetPollTargetClamps: the estimator only deepens, never thins below
// the proven baseline, and never exceeds the spam bound.
func TestSetPollTargetClamps(t *testing.T) {
	defer pollTarget.Store(int64(pollLimit))
	SetPollTarget(1)
	if got := PollTarget(); got != pollLimit {
		t.Fatalf("target=%d after Set(1), want baseline %d", got, pollLimit)
	}
	SetPollTarget(1 << 20)
	if got := PollTarget(); got != maxPollTarget {
		t.Fatalf("target=%d after Set(huge), want cap %d", got, maxPollTarget)
	}
	SetPollTarget(200)
	if got := PollTarget(); got != 200 {
		t.Fatalf("target=%d after Set(200), want 200", got)
	}
}

// TestMonitorResponseStats: data responses feed the mean size and inflight
// accounting the poll gate budgets against; empty polls count bytes but
// must not drag the mean.
func TestMonitorResponseStats(t *testing.T) {
	m := newBBRMonitor()
	if got := m.AvgResponse(); got != 0 {
		t.Fatalf("avg=%d before data, want 0", got)
	}
	m.sent(100, 7)
	m.sent(100, 8)
	if got := m.Inflight(); got != 2 {
		t.Fatalf("inflight=%d, want 2", got)
	}
	m.receivedByID(16000, 7)
	m.receivedByID(0, 8) // empty poll response: bytes yes, mean no
	if got := m.Inflight(); got != 0 {
		t.Fatalf("inflight=%d after answers, want 0", got)
	}
	if got := m.AvgResponse(); got != 16000 {
		t.Fatalf("avg=%d, want 16000 (empty must not drag it)", got)
	}
}
