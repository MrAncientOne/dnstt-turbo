package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
)

func allPackets(buf []byte) ([][]byte, error) {
	var packets [][]byte
	r := bytes.NewReader(buf)
	for {
		p, err := nextPacket(r)
		if err != nil {
			return packets, err
		}
		packets = append(packets, p)
	}
}

func packetsEqual(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestNextPacket(t *testing.T) {
	for _, test := range []struct {
		input   string
		packets [][]byte
		err     error
	}{
		{"", [][]byte{}, io.EOF},
		{"\x00", [][]byte{}, io.ErrUnexpectedEOF},
		{"\x00\x00", [][]byte{{}}, io.EOF},
		{"\x00\x00\x00", [][]byte{{}}, io.ErrUnexpectedEOF},
		{"\x00\x01", [][]byte{}, io.ErrUnexpectedEOF},
		{"\x00\x05hello\x00\x05world", [][]byte{[]byte("hello"), []byte("world")}, io.EOF},
	} {
		packets, err := allPackets([]byte(test.input))
		if !packetsEqual(packets, test.packets) || err != test.err {
			t.Errorf("%x\nreturned %x %v\nexpected %x %v",
				test.input, packets, err, test.packets, test.err)
		}
	}
}

func TestBBRMonitorAccounting(t *testing.T) {
	m := newBBRMonitor()
	a := m.reserveID()
	m.sent(300, a)
	time.Sleep(60 * time.Millisecond) // ordered path delay
	b := m.reserveID()
	if b == a {
		t.Fatal("reserveID reused an in-flight ID")
	}
	m.sent(300, b)
	time.Sleep(80 * time.Millisecond) // true ~80-140ms exchange
	m.receivedByID(1000, a)
	m.receivedByID(500, b)
	s := m.tick()
	if s.DeliveredBytes != 1500 {
		t.Fatalf("delivered %d, want 1500", s.DeliveredBytes)
	}
	if s.SentBytes != 600 {
		t.Fatalf("sent %d, want 600", s.SentBytes)
	}
	if s.AppLimited {
		t.Fatal("active flow marked app-limited")
	}
	if !s.HasRTT || s.RTT < 10*time.Millisecond {
		t.Fatalf("matched exchange produced no usable RTT sample: %+v", s)
	}
	// Idle tick: nothing delivered, nothing in flight => app-limited.
	s = m.tick()
	if !s.AppLimited {
		t.Fatal("idle tick not marked app-limited")
	}
	if s.DeliveredBytes != 0 {
		t.Fatalf("delivered %d after reset, want 0", s.DeliveredBytes)
	}
}

func TestBBRMonitorOutOfOrderMatches(t *testing.T) {
	m := newBBRMonitor()
	var ids []uint16
	for i := 0; i < 10; i++ {
		id := m.reserveID()
		ids = append(ids, id)
		m.sent(300, id)
		time.Sleep(8 * time.Millisecond) // 80ms of queue ahead
	}
	// Answer newest-first (server out-of-order per RFC 7766 section 7):
	// every response still matches its own send exactly.
	for i := 9; i >= 0; i-- {
		time.Sleep(2 * time.Millisecond)
		m.receivedByID(100, ids[i])
	}
	s := m.tick()
	if s.DeliveredBytes != 1000 {
		t.Fatalf("delivered %d, want 1000", s.DeliveredBytes)
	}
	if !s.HasRTT {
		t.Fatal("exact matches under out-of-order answers produced no sample")
	}
	// Newest answers matched the youngest sends (~20ms), oldest ~100ms;
	// the tick minimum must be a young match, never queue position.
	if s.RTT > 60*time.Millisecond {
		t.Fatalf("tick RTT %v looks like queue position, want a young match", s.RTT)
	}
	if s.AppLimited {
		t.Fatal("drained flow marked app-limited on an active tick")
	}
}

func TestBBRMonitorDuplicateIDFIFO(t *testing.T) {
	m := newBBRMonitor()
	// Force a duplicate ID: two sends share one bucket, pair FIFO.
	m.sent(300, 4242)
	time.Sleep(60 * time.Millisecond)
	m.sent(300, 4242)
	time.Sleep(60 * time.Millisecond) // ~120ms vs ~60ms
	m.receivedByID(100, 4242)         // answers the older send
	m.receivedByID(100, 4242)         // answers the younger send
	s := m.tick()
	if s.DeliveredBytes != 200 {
		t.Fatalf("delivered %d, want 200", s.DeliveredBytes)
	}
	if !s.HasRTT {
		t.Fatal("duplicate-ID exchange produced no sample")
	}
	if s.RTT < 10*time.Millisecond || s.RTT > 90*time.Millisecond {
		t.Fatalf("tick RTT %v, want the younger ~60ms match", s.RTT)
	}
}

func TestBBRMonitorUnknownIDNoSample(t *testing.T) {
	m := newBBRMonitor()
	id := m.reserveID()
	m.sent(300, id)
	// Answer for a query never sent (stale sweeps, pre-drop connection):
	// bytes count, no sample, outstanding send untouched.
	m.receivedByID(700, id+1)
	s := m.tick()
	if s.DeliveredBytes != 700 {
		t.Fatalf("delivered %d, want 700", s.DeliveredBytes)
	}
	if s.HasRTT {
		t.Fatalf("unknown-ID response produced sample %v", s.RTT)
	}
	if s.AppLimited {
		t.Fatal("outstanding query marked app-limited")
	}
}

func TestBBRMonitorReconnectSweep(t *testing.T) {
	m := newBBRMonitor()
	for i := 0; i < 2000; i++ {
		m.sent(300, m.reserveID())
	}
	// No answers ever arrive (TCP drop): a tick sweep only expires 30s-old
	// entries, so the table stays non-empty and reports stalled.
	s := m.tick()
	if s.AppLimited {
		t.Fatal("2000 orphaned sends marked app-limited; must report stalled")
	}
	m.mu.Lock()
	n := m.pendingCount
	m.mu.Unlock()
	if n != 2000 {
		t.Fatalf("pending %d, want 2000 retained within TTL", n)
	}
}

func TestBBRMonitorFloorRejectsSpoof(t *testing.T) {
	m := newBBRMonitor()
	id := m.reserveID()
	m.sent(300, id)
	time.Sleep(time.Millisecond) // loopback-speed coincidence, not a path
	m.receivedByID(100, id)
	s := m.tick()
	if s.HasRTT {
		t.Fatalf("sub-floor %v sample admitted, must be rejected", s.RTT)
	}
	if s.DeliveredBytes != 100 {
		t.Fatalf("delivered %d, want 100", s.DeliveredBytes)
	}
}

func TestBBRMonitorCarryOverStall(t *testing.T) {
	m := newBBRMonitor()
	id := m.reserveID()
	m.sent(300, id)
	// Stalled ticks carry: no sample emitted, only stall-vs-idle reported.
	for i := 0; i < maxCarryTicks; i++ {
		s := m.tick()
		if s.Interval != 0 {
			t.Fatalf("tick %d: stalled tick emitted interval %v, want carry", i, s.Interval)
		}
		if s.AppLimited {
			t.Fatalf("tick %d: outstanding query marked app-limited", i)
		}
	}
	// Past the cap: exactly one zero-rate stalled sample, then carry again.
	s := m.tick()
	if s.Interval <= 0 {
		t.Fatal("post-cap stall emitted nothing")
	}
	if s.AppLimited || s.DeliveredBytes != 0 {
		t.Fatalf("post-cap stall sample %+v, want stalled zero-rate", s)
	}
	s = m.tick()
	if s.Interval != 0 {
		t.Fatalf("expected carry to resume, got interval %v", s.Interval)
	}
	// Answering drains everything back to idle.
	m.receivedByID(0, id)
	s = m.tick()
	if s.DeliveredBytes != 0 {
		t.Fatalf("delivered %d, want 0", s.DeliveredBytes)
	}
	s = m.tick()
	if !s.AppLimited {
		t.Fatal("drained flow not marked app-limited")
	}
}

func TestBBRMonitorCarryMergesDelivery(t *testing.T) {
	m := newBBRMonitor()
	id := m.reserveID()
	m.sent(300, id)
	if s := m.tick(); s.Interval != 0 {
		t.Fatalf("expected carry, got %+v", s)
	}
	time.Sleep(time.Millisecond)
	m.receivedByID(1000, id)
	s := m.tick()
	if s.DeliveredBytes != 1000 {
		t.Fatalf("delivered %d, want merged 1000", s.DeliveredBytes)
	}
	if s.Interval <= 0 {
		t.Fatal("merged sample has no interval")
	}
	if s.AppLimited {
		t.Fatal("merged delivery marked app-limited")
	}
}

func TestBBRMonitorStalledVsIdle(t *testing.T) {
	m := newBBRMonitor()
	// Stalled: sent, never answered, nothing delivered.
	id := m.reserveID()
	m.sent(300, id)
	s := m.tick()
	if s.AppLimited {
		t.Fatal("outstanding query marked app-limited; stall must track down")
	}
	// Idle: answer it, then tick empty.
	m.receivedByID(0, id)
	s = m.tick()
	if s.DeliveredBytes != 0 {
		t.Fatalf("delivered %d, want 0", s.DeliveredBytes)
	}
	s = m.tick()
	if !s.AppLimited {
		t.Fatal("empty queue tick not marked app-limited")
	}
}

// TestEstimatorSeesThroughBursts is the end-to-end regression for the live
// failure where the app moved ~1MB/s while the estimator read median zero:
// delivery arrives in clumps with empty ticks between, and fixed ticking
// used to mint a zero-rate point per empty tick. Twenty groups of
// (send, carry, carry, 90KB delivered) must read ~1MB/s in both max and
// sustained, never zero. Needs real sleeps: the monitor clocks wall time.
func TestEstimatorSeesThroughBursts(t *testing.T) {
	m := newBBRMonitor()
	ctl := bbr.NewController(bbr.Config{
		SeedBWBps: 250000,
		SeedRTT:   100 * time.Millisecond,
		MinSegs:   16,
		MaxSegs:   2048,
		SegBytes:  1200,
	})
	feed := func(s bbr.Sample) {
		if s.Interval > 0 {
			ctl.Update(s)
		}
	}
	for g := 0; g < 20; g++ {
		id := m.reserveID()
		m.sent(300, id)
		feed(m.tick())
		time.Sleep(30 * time.Millisecond)
		feed(m.tick())
		time.Sleep(30 * time.Millisecond)
		m.receivedByID(90000, id)
		feed(m.tick())
		time.Sleep(30 * time.Millisecond)
	}
	if bw := ctl.MaxBW(); bw < 300000 || bw > 5000000 {
		t.Fatalf("maxBW %.0f, want near 1MB/s aggregate", bw)
	}
	if sus := ctl.SustainedBW(); sus < 300000 {
		t.Fatalf("sustained %.0f, want near 1MB/s (was exactly 0 pre-fix)", sus)
	}
}

func TestBBRMonitorRandomizedMatching(t *testing.T) {
	// Randomized exact matching: N sends with reserved IDs over a
	// 60-100ms path with jitter, answers shuffled out of order (RFC 7766
	// section 7). Tick minimum must land near the true base RTT, never
	// below the floor, bytes exact, regardless of answer order.
	for seed := int64(1); seed <= 20; seed++ {
		m := newBBRMonitor()
		var sentIDs []uint16
		for i := 0; i < 30; i++ {
			id := m.reserveID()
			sentIDs = append(sentIDs, id)
			m.sent(300, id)
			time.Sleep(5 * time.Millisecond)
		}
		// Answer in reverse (worst-case reorder) after path delay.
		time.Sleep(60*time.Millisecond + time.Duration(seed%40)*time.Millisecond)
		var wantDelivered uint64
		for i := len(sentIDs) - 1; i >= 0; i-- {
			m.receivedByID(1000, sentIDs[i])
			wantDelivered += 1000
		}
		s := m.tick()
		if s.DeliveredBytes != wantDelivered {
			t.Fatalf("seed %d: delivered %d, want %d", seed, s.DeliveredBytes, wantDelivered)
		}
		if !s.HasRTT {
			t.Fatalf("seed %d: no RTT from matched exchange", seed)
		}
		if s.RTT < 10*time.Millisecond || s.RTT > 5*time.Second {
			t.Fatalf("seed %d: tick RTT %v outside physical bounds", seed, s.RTT)
		}
		if s.AppLimited {
			t.Fatalf("seed %d: backlogged flow marked app-limited", seed)
		}
	}
}
