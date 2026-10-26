// Package diagnostics records process-local health without inspecting traffic.
package diagnostics

import (
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Health struct {
	Role                                          string
	TxBytes, RxBytes, TxFrames, RxFrames          atomic.Uint64
	DialAttempts, DialFailures, Failures          atomic.Uint64
	ActiveCarriers, ActiveSessions, ActiveStreams atomic.Int64
	UpstreamAttempts, UpstreamFailures            atomic.Uint64
	ActiveUpstreams                               atomic.Int64
	LastReceive                                   atomic.Int64
	mu                                            sync.Mutex
	last                                          map[string]time.Time
	counts                                        map[string]uint64
}

func New(role string) *Health {
	return &Health{Role: role, last: make(map[string]time.Time), counts: make(map[string]uint64)}
}

// Failure counts every occurrence, but emits at most one line per event type
// per 30 seconds. Callers use fixed event names, never packet data or keys.
func (h *Health) Failure(event string, err error) {
	h.Failures.Add(1)
	h.mu.Lock()
	h.counts[event]++
	count := h.counts[event]
	now := time.Now()
	last := h.last[event]
	emit := last.IsZero() || now.Sub(last) >= 30*time.Second
	if emit {
		h.last[event] = now
	}
	h.mu.Unlock()
	if emit {
		log.Printf("role=%s event=%s count=%d error=%q", h.Role, event, count, err)
	}
}

func (h *Health) Received(n int) {
	h.RxFrames.Add(1)
	h.RxBytes.Add(uint64(n))
	h.LastReceive.Store(time.Now().UnixMilli())
}

func (h *Health) Summary() {
	idle := int64(-1)
	if last := h.LastReceive.Load(); last != 0 {
		idle = time.Now().UnixMilli() - last
	}
	log.Printf("role=%s event=health carriers=%d sessions=%d streams=%d dial_attempts=%d dial_failures=%d failures=%d tx_frames=%d rx_frames=%d tx_bytes=%d rx_bytes=%d rx_idle_ms=%d upstream_attempts=%d upstream_failures=%d upstream_active=%d",
		h.Role, h.ActiveCarriers.Load(), h.ActiveSessions.Load(), h.ActiveStreams.Load(),
		h.DialAttempts.Load(), h.DialFailures.Load(), h.Failures.Load(), h.TxFrames.Load(), h.RxFrames.Load(), h.TxBytes.Load(), h.RxBytes.Load(), idle, h.UpstreamAttempts.Load(), h.UpstreamFailures.Load(), h.ActiveUpstreams.Load())
	h.mu.Lock()
	counts := make(map[string]uint64, len(h.counts))
	var events []string
	for event, n := range h.counts {
		events = append(events, event)
		counts[event] = n
	}
	h.mu.Unlock()
	sort.Strings(events)
	for _, event := range events {
		log.Printf("role=%s event=failure_count kind=%s count=%d", h.Role, event, counts[event])
	}
}

// Start returns an idempotent stop function that joins the reporter and emits
// a final summary. The caller owns its lifetime, including failed handshakes.
func (h *Health) Start() func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(stopped)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				h.Summary()
			}
		}
	}()
	return func() { once.Do(func() { close(done); <-stopped; h.Summary() }) }
}
