package main

import (
	"testing"
	"time"
)

// testMonitor returns a monitor driven by a manual clock starting at unix 0.
func testMonitor() (*bbrMonitor, *manualClock) {
	mc := &manualClock{t: time.Unix(0, 0)}
	m := newBBRMonitor()
	m.now = mc.now
	m.tickStart = mc.now()
	return m, mc
}

type manualClock struct{ t time.Time }

func (c *manualClock) now() time.Time { return c.t }
func (c *manualClock) step(d time.Duration) {
	c.t = c.t.Add(d)
}

// TestMonitorCarryCapByTicksNotWall: carry-over is bounded by tick count,
// not wall time — a single tick after a 10-minute gap must emit, not carry
// forever.
func TestMonitorCarryCapByTicksNotWall(t *testing.T) {
	m, mc := testMonitor()
	m.sent(300, 4242)
	for i := 0; i < maxCarryTicks; i++ {
		mc.step(time.Millisecond)
		if s := m.tick(); s.Interval != 0 {
			t.Fatalf("tick %d: want carry, got %+v", i, s)
		}
	}
	// Jump the clock near (but inside) the entry TTL: the cap is
	// tick-counted, so this tick emits a stalled sample rather than
	// carrying on wall time.
	mc.step(25 * time.Second)
	s := m.tick()
	if s.Interval <= 0 {
		t.Fatal("post-cap tick after clock jump emitted nothing")
	}
	if s.AppLimited || s.DeliveredBytes != 0 {
		t.Fatalf("want stalled zero-rate sample, got %+v", s)
	}
}

// TestMonitorTTLSweepExpires: entries older than monitorEntryTTL are swept
// on tick, bounding memory after a reconnect that orphans sends.
func TestMonitorTTLSweepExpires(t *testing.T) {
	m, mc := testMonitor()
	for i := 0; i < 100; i++ {
		m.sent(300, uint16(i))
	}
	mc.step(monitorEntryTTL + time.Second)
	s := m.tick()
	// All entries swept: nothing outstanding, nothing delivered => idle.
	if !s.AppLimited {
		t.Fatalf("post-TTL tick %+v, want app-limited idle", s)
	}
	m.mu.Lock()
	n := m.pendingCount
	left := len(m.pending)
	m.mu.Unlock()
	if n != 0 || left != 0 {
		t.Fatalf("pendingCount=%d map=%d after TTL sweep, want 0/0", n, left)
	}
}

// TestMonitorOverflowBound: with zero answers, in-flight accounting stays
// capped at maxMonitorInflight and keeps reporting stalled, never idle.
func TestMonitorOverflowBound(t *testing.T) {
	m, mc := testMonitor()
	for i := 0; i < 3*maxMonitorInflight; i++ {
		m.sent(300, uint16(i%55537))
		mc.step(time.Millisecond)
	}
	m.mu.Lock()
	n := m.pendingCount
	m.mu.Unlock()
	if n > maxMonitorInflight {
		t.Fatalf("pending %d exceeds cap %d", n, maxMonitorInflight)
	}
	mc.step(100 * time.Millisecond)
	for i := 0; i < maxCarryTicks+1; i++ {
		mc.step(100 * time.Millisecond)
	}
	s := m.tick()
	if s.AppLimited {
		t.Fatal("capped-but-unanswered table marked app-limited; must report stalled")
	}
}

// TestMonitorReserveExhaustionFallsBack: when every ID is outstanding,
// reserveID still returns (bounded tries) and per-ID FIFOs keep pairing
// correct.
func TestMonitorReserveExhaustionFallsBack(t *testing.T) {
	m, _ := testMonitor()
	// Occupy a dense run of IDs directly (bypass reserveID's avoidance).
	for i := 0; i < 300; i++ {
		m.sent(10, uint16(i))
	}
	done := make(chan uint16, 1)
	go func() { done <- m.reserveID() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reserveID hung on a crowded table")
	}
	// Duplicate answers still pair FIFO and count bytes exactly.
	m.receivedByID(100, 7)
	m.receivedByID(200, 7)
	s := m.tick()
	if s.DeliveredBytes != 300 {
		t.Fatalf("delivered %d, want 300", s.DeliveredBytes)
	}
}

// TestMonitorIntervalSpansCarry: a delivery landing after carried ticks
// produces one sample whose interval covers the whole stall (no zero-rate
// artifact, no lost bytes).
func TestMonitorIntervalSpansCarry(t *testing.T) {
	m, mc := testMonitor()
	id := uint16(99)
	m.sent(300, id)
	mc.step(100 * time.Millisecond)
	if s := m.tick(); s.Interval != 0 {
		t.Fatalf("want carry, got %+v", s)
	}
	mc.step(900 * time.Millisecond)
	m.receivedByID(5000, id)
	s := m.tick()
	if s.DeliveredBytes != 5000 {
		t.Fatalf("delivered %d, want 5000 merged", s.DeliveredBytes)
	}
	if s.Interval < time.Second {
		t.Fatalf("interval %v, want >= 1s spanning the stall", s.Interval)
	}
	rate := float64(s.DeliveredBytes) / s.Interval.Seconds()
	if rate < 4000 || rate > 6000 {
		t.Fatalf("merged rate %.0f B/s, want ~5000", rate)
	}
}

// TestMonitorRTTBounds: below-floor and absurd samples never reach the tick
// minimum; the in-band minimum does.
func TestMonitorRTTBounds(t *testing.T) {
	m, mc := testMonitor()
	m.sent(300, 1)
	mc.step(time.Microsecond)
	m.receivedByID(10, 1) // sub-floor: scheduling coincidence
	m.sent(300, 2)
	mc.step(100 * time.Millisecond)
	m.receivedByID(10, 2) // in-band
	s := m.tick()
	if !s.HasRTT {
		t.Fatal("in-band match produced no sample")
	}
	if s.RTT < minRTTSampleFloor || s.RTT > 500*time.Millisecond {
		t.Fatalf("tick RTT %v, want the ~100ms in-band match", s.RTT)
	}
}

// TestMonitorOrderCompaction: the send-order log compacts instead of growing
// without bound under sustained churn.
func TestMonitorOrderCompaction(t *testing.T) {
	m, mc := testMonitor()
	for i := 0; i < 20000; i++ {
		id := uint16(i % 60000)
		m.sent(10, id)
		m.receivedByID(10, id)
		mc.step(time.Millisecond)
		if i%100 == 0 {
			m.tick()
		}
	}
	m.mu.Lock()
	orderLen := len(m.order)
	head := m.orderHead
	m.mu.Unlock()
	if orderLen > 8192 || head > 4096 {
		t.Fatalf("order log len=%d head=%d after 20k exchanges: compaction failed", orderLen, head)
	}
}
