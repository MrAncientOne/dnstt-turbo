package fairqueue

import (
	"testing"
	"time"
)

// TestShaperGatesDelivery: with a slow shaped rate, back-to-back chunks must
// be spaced by the serialisation delay (deficit-mode shaper,
// sch_cake.c:cake_dequeue gate). Unlimited (default) delivers immediately.
func TestShaperGatesDelivery(t *testing.T) {
	cfg := testCfg()
	cfg.RateBps = 200_000 // 200KB/s: 1KB chunk ~= 5ms serialisation
	cfg.Chunk = 1024
	s := NewScheduler(cfg)
	go s.Run()
	defer s.Close()
	w := &scriptWriter{}
	f := s.AddFlow(w)
	const chunks = 4
	done := make(chan error, 1)
	go func() {
		for i := 0; i < chunks; i++ {
			if err := s.Submit(f, make([]byte, 1024)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	t0 := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("submit: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("shaped delivery hung")
	}
	el := time.Since(t0)
	// 4KB at 200KB/s ~= 20ms of serialisation; demand clear spacing with
	// wide margins (loaded CI), and an upper bound proving no wedge.
	if el < 10*time.Millisecond {
		t.Fatalf("4 chunks delivered in %v with 200KB/s shaper (gate open?)", el)
	}
	if el > 20*time.Second {
		t.Fatalf("shaped delivery took %v (wedged?)", el)
	}
}

// TestOverlimitVictimizesBulk: over the buffer limit, the longest bulk
// queue is dropped first (sch_cake.c:cake_drop) and its owner observes
// ErrBusy; a longer incoming bulk chunk is tail-dropped instead.
func TestOverlimitVictimizesBulk(t *testing.T) {
	s := NewScheduler(testCfg())
	s.mu.Lock()
	s.bufferLimit = 3000
	now := time.Now()
	mk := func(n int, bulk bool) *Flow {
		f := &Flow{pending: make([]byte, n), enqueueT: now, deficit: 10000, freeCh: make(chan struct{}, 1), progress: make(chan struct{}, 1)}
		f.inQueue = true
		if bulk {
			f.set = setBulk
			s.ring = append(s.ring, f)
		} else {
			f.set = setSparse
			s.sparse = append(s.sparse, f)
		}
		s.bufferUsed += uint64(n)
		return f
	}
	big := mk(1000, true)
	mk(200, true)
	mk(100, false)
	// Room: drop the 1000B bulk victim to admit a 500B chunk.
	if !s.dropLongestLocked(now, make([]byte, 500)) {
		s.mu.Unlock()
		t.Fatal("expected a bulk victim to be dropped")
	}
	if !big.dead || !big.dropBusy {
		s.mu.Unlock()
		t.Fatal("longest bulk flow not victimized")
	}
	select {
	case <-big.freeCh:
	default:
		s.mu.Unlock()
		t.Fatal("victim owner not released")
	}
	if s.dropped != 1 {
		s.mu.Unlock()
		t.Fatalf("dropped=%d, want 1", s.dropped)
	}
	// Incoming longer than any queued bulk: tail-drop the incoming.
	if s.dropLongestLocked(now, make([]byte, 2000)) {
		s.mu.Unlock()
		t.Fatal("incoming longest must be tail-dropped, not admitted")
	}
	s.mu.Unlock()
}

// TestDeficitByteFairness: two continuously-backlogged bulk flows with 4:1
// chunk sizes must share bytes within a bounded ratio (DRR++, not unit RR:
// unit RR would give exactly 4:1).
func TestDeficitByteFairness(t *testing.T) {
	s := NewScheduler(testCfg()) // unlimited: quantum 1514
	a := s.AddFlow(nil)
	b := s.AddFlow(nil)
	s.mu.Lock()
	serve := func(f *Flow, n int) {
		f.pending = make([]byte, n)
		f.ready = true
		if !f.inQueue {
			f.inQueue = true
			f.set = setBulk
			s.ring = append(s.ring, f)
		}
	}
	serve(a, 4000)
	serve(b, 1000)
	var bytesA, bytesB int
	for i := 0; i < 60; i++ {
		f, c := s.nextLocked()
		if f == nil {
			s.mu.Unlock()
			t.Fatal("scheduler went idle with both flows backlogged")
		}
		switch f {
		case a:
			bytesA += len(c)
			serve(a, 4000)
		case b:
			bytesB += len(c)
			serve(b, 1000)
		default:
			s.mu.Unlock()
			t.Fatal("unknown flow served")
		}
	}
	s.mu.Unlock()
	if bytesA == 0 || bytesB == 0 {
		t.Fatalf("starvation: bytesA=%d bytesB=%d", bytesA, bytesB)
	}
	ratio := float64(bytesA) / float64(bytesB)
	// Unit round-robin would give exactly 4.0; DRR++ must do strictly
	// better (byte-fair within a few quanta).
	if ratio < 1.0/3 || ratio > 3.0 {
		t.Fatalf("byte ratio A/B=%.2f, want within [1/3,3]", ratio)
	}
	t.Logf("byte ratio A/B=%.2f over 60 turns (unit RR would be 4.0)", ratio)
}
