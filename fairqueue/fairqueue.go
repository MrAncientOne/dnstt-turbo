// Package fairqueue implements a single-writer fair scheduler that paces
// many flows into one smux session.
//
// Problem: on either direction of a multiplexed tunnel, every stream's
// goroutine normally copies greedily into its smux stream, so a bulk flood
// (a BitTorrent swarm = thousands of streams) dominates the one shared pipe
// and starves interactive/DNS streams. The sender is the only place order
// can actually be controlled, so this package interposes a single writer in
// front of smux. Per-flow readers pull from their source in small chunks and
// hand exactly one chunk at a time to the scheduler (blocking until it is
// written — the backpressure that makes a de-prioritized bulk flow stop
// reading its socket instead of buffering in RAM). The writer serves:
//
//  1. SPARSE flows first — a flow idle longer than Config.Idle before it
//     produced this chunk (DNS, SSH, page control bytes, API calls). This is
//     fq_codel's new-flow priority (RFC 8290): classified by *behaviour*,
//     not app or byte-count, so it is immune to a bulk app spreading load
//     over thousands of connections. At most Config.MaxBurst sparse turns run
//     back-to-back before one ring turn is forced, so a storm of fresh
//     connections cannot starve bulk forever (the RFC's starvation caveat;
//     MaxBurst <= 0 restores strict priority).
//  2. Otherwise deficit round-robin over active flows (CAKE DRR++,
//
// /     sch_cake.c:cake_dequeue): each flow holds a deficit topped up by the
//
//	flow quantum, debited by bytes served, so bulk-vs-bulk shares
//	byte-fair rather than chunk-fair.
//
// Scheduling discipline is CAKE (single BESTEFFORT tin,
// sch_cake.c:cake_config_besteffort, vendored read-only at
// ../third_party/cake/sch_cake.c; Go mechanisms in ../cake):
//
//   - DRR++ deficit with flow quantum from the shaper rate
//     (cake.ComputeRateParams, i.e. sch_cake.c:cake_set_rate). Quantum is
//     per-flow state that persists across turns; new flows start full.
//   - Cobalt AQM state per flow (sch_cake.c:cobalt_should_drop). At the
//     tunnel's depth of one pending chunk per flow the kernel's last-packet
//     rule always delivers, so Cobalt observes but never drops: drops come
//     from the buffer overlimit and the starvation timer, both of which fail
//     the owning Submit fast (end-to-end TCP retransmits; no bytes die in
//     RAM). If batching ever deepens per-flow queues, the AQM path already
//     charges correctly.
//   - Deficit-mode shaper (sch_cake.c:cake_advance_shaper, no burst
//     parameter) driven by SetRate — normally BBR's SustainedBW on the
//     client upload path, which replaces the external cake-autorate.sh.
//     Rate 0 (default, and the server download path which has no estimator)
//     means unlimited: fairness without a ceiling.
//   - Buffer overlimit (sch_cake.c:cake_drop / cake_reconfigure sizing):
//     total pending bytes are capped (default 4MiB, rate-scaled above it);
//     overlimit drops the longest bulk queue first so a flood sacrifices one
//     torrent peer connection instead of an interactive chunk.
//
// Three bounds keep a storm from wedging the scheduler: the sparse list is
// capped at Config.MaxSparse (overflow demotes to the ring), and a submit
// that is not served within Config.SubmitTimeout fails fast with ErrBusy
// instead of parking forever, so the queue can never grow without bound and
// a stall becomes a quick retry. A per-chunk write deadline keeps one slow
// peer from head-of-line-blocking the single writer.
//
// Both tunnel directions share this one implementation: the client paces
// uploads with it, the server paces downloads.
//
// The zero value is not usable; construct with NewScheduler. The constructor
// does NOT start the writer; the caller runs go s.Run(). This keeps the
// scheduling policy drivable by hand in tests.
package fairqueue

import (
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"

	"www.bamsoftware.com/git/dnstt.git/cake"
)

// Defaults mirror the values proven on a real ~11/7 Mbps DNS tunnel.
const (
	DefaultChunk         = 4096
	DefaultIdle          = 100 * time.Millisecond
	DefaultWriteTimeout  = 500 * time.Millisecond
	DefaultMaxBurst      = 8
	DefaultMaxSparse     = 64
	DefaultSubmitTimeout = 250 * time.Millisecond
	// DefaultLinger bounds the graceful-close wait (see Linger).
	DefaultLinger = 250 * time.Millisecond
	// DefaultRTT seeds the CoDel interval auto-tuning until SetRate feeds
	// a measured RTT (sch_cake.c:cake_config_init interval 100ms).
	DefaultRTT = 100 * time.Millisecond
)

// Flow sets mirror sch_cake.c CAKE_SET_* for the single tin. The tunnel
// needs only three: none (not queued), sparse (new_flows) and bulk
// (old_flows). There is no decaying list: the tunnel's idle-gap detector
// replaces the kernel's empty-state heuristic, and at depth one the AQM
// never drops, so there is no decaying AQM state to retain. No host
// isolation, no Diffserv tins (single BESTEFFORT).
const (
	setNone uint8 = iota
	setSparse
	setBulk
)

// Config tunes a Scheduler. All fields must be positive except MaxBurst,
// where <= 0 restores strict sparse priority (bulk can starve under a storm),
// and RateBps, where 0 means unlimited (fairness without a ceiling).
type Config struct {
	// Chunk caps bytes read/written per flow per turn.
	Chunk int
	// Idle is the quiet gap that (re)marks a flow sparse (interactive).
	Idle time.Duration
	// WriteTimeout bounds one scheduler turn. For smux, expiry yields while
	// the same write continues; the next turn awaits it without resending.
	WriteTimeout time.Duration
	// MaxBurst caps consecutive sparse turns before one ring turn is forced.
	MaxBurst int
	// MaxSparse caps the sparse list; overflow demotes to the ring.
	MaxSparse int
	// SubmitTimeout bounds a chunk's wait for service; expiry fails fast.
	SubmitTimeout time.Duration
	// Reliable applies backpressure when the queue is full and waits through
	// congestion rather than terminating a stream for scheduler delay.
	Reliable bool
	// Linger caps the graceful-close wait (see Linger). <=0 disables.
	Linger time.Duration
	// RateBps is the shaper rate in bytes/second for the deficit-mode
	// shaper (0 = unlimited). Normally set live via SetRate from BBR's
	// SustainedBW; the static value here only seeds startup.
	RateBps uint64
	// RTT seeds the CoDel interval auto-tuning (see SetRate).
	RTT time.Duration
	// MTU is the serialisation-delay basis for the auto-tuning (0 = Chunk).
	MTU uint32
}

// DefaultConfig returns the proven defaults.
func DefaultConfig() Config {
	return Config{
		Chunk:         DefaultChunk,
		Idle:          DefaultIdle,
		WriteTimeout:  DefaultWriteTimeout,
		MaxBurst:      DefaultMaxBurst,
		MaxSparse:     DefaultMaxSparse,
		SubmitTimeout: DefaultSubmitTimeout,
		Linger:        DefaultLinger,
		RateBps:       0,
		RTT:           DefaultRTT,
		MTU:           0,
	}
}

// ErrFlowClosed reports use of a torn-down flow or scheduler.
var ErrFlowClosed = errors.New("fairqueue: flow closed")

// ErrBusy reports a submit that waited SubmitTimeout without service, or a
// chunk dropped by the buffer overlimit. The caller tears the flow down;
// the app sees a fast failure and retries instead of a stall.
var ErrBusy = errors.New("fairqueue: scheduler saturated")

// StreamWriter is the slice of *smux.Stream the writer needs: one bounded
// write per turn. An interface (satisfied by *smux.Stream) so tests can
// inject faulty writers (short writes, slow peers, fatal errors) without
// a live session. Callers keep passing *smux.Stream unchanged.
type StreamWriter interface {
	Write([]byte) (int, error)
	SetWriteDeadline(time.Time) error
}

// Flow is one stream registered with a Scheduler.
type Flow struct {
	stream  StreamWriter
	pending []byte
	// enqueueT stamps Submit arrival for the Cobalt sojourn measurement
	// (sch_cake.c:cobalt_get_enqueue_time).
	enqueueT time.Time
	// deficit is the DRR++ deficit (sch_cake.c:cake_flow.deficit),
	// debited by bytes served, topped up by the flow quantum. It
	// persists across turns; new flows start full.
	deficit int32
	// cvars is the per-flow Cobalt AQM state
	// (sch_cake.c:cake_flow.cvars).
	cvars   cake.CobaltVars
	set     uint8
	ready   bool
	inQueue bool
	dead    bool
	serving bool // selected by Run; SubmitTimeout only covers queued time
	// dropBusy marks a flow victimized by the buffer overlimit: parked in
	// Submit, it must observe ErrBusy (not ErrFlowClosed) when released.
	dropBusy bool
	lastSeen time.Time
	freeCh   chan struct{}
	// progress is signaled (non-blocking) every time the writer picks this
	// flow up but has to requeue it (slow peer / full window): proof the
	// flow is being served, just slowly. It resets the submit timeout, so
	// the timeout only fires on scheduler starvation (never picked), never
	// on a saturated-but-moving pipe. Buffered(1); orphaned tokens are GC'd.
	progress chan struct{}
	// lastServe records the last successful chunk write (UnixNano, atomic:
	// written by the writer, read by Linger without the mutex).
	lastServe atomic.Int64
}

// Stats snapshots scheduler counters. All fields are cumulative.
type Stats struct {
	SparseTurns uint64 // chunks served from the sparse list
	BulkTurns   uint64 // chunks served from the ring
	Demoted     uint64 // sparse submits demoted to the ring (list full)
	TimedOut    uint64 // submits that expired and failed fast
	Dropped     uint64 // chunks dropped by the buffer overlimit
}

// Scheduler is one fair writer for one smux session.
type Scheduler struct {
	cfg Config

	mu     sync.Mutex
	wake   chan struct{} // buffered(1); readers nudge the writer
	done   chan struct{}
	space  chan struct{} // broadcast when queue capacity or flow state changes
	sparse []*Flow       // new_flows: priority FIFO of newly-active flows
	ring   []*Flow       // old_flows: DRR of active bulk flows
	rr     int
	closed bool
	// consecSparse counts back-to-back sparse turns (reserve enforcement).
	consecSparse int
	// CAKE tin state (single BESTEFFORT tin).
	quantum     uint16
	cparams     cake.CobaltParams
	shaper      cake.Shaper
	bufferUsed  uint64
	bufferLimit uint64
	rng         *rand.Rand
	rngMu       sync.Mutex
	// Counters for tuning/observability. Guarded by mu.
	sparseTurns uint64
	bulkTurns   uint64
	demoted     uint64
	timedOut    uint64
	dropped     uint64
}

// NewScheduler builds a Scheduler. Call go s.Run() to start the writer.
func NewScheduler(cfg Config) *Scheduler {
	if cfg.Chunk <= 0 {
		cfg.Chunk = DefaultChunk
	}
	if cfg.Idle <= 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.MaxSparse < 0 {
		cfg.MaxSparse = 0
	}
	if cfg.SubmitTimeout <= 0 {
		cfg.SubmitTimeout = DefaultSubmitTimeout
	}
	if cfg.RTT <= 0 {
		cfg.RTT = DefaultRTT
	}
	// MaxBurst <= 0 is meaningful (strict priority): keep as given.
	// RateBps 0 (unlimited) and MTU 0 (=Chunk) are meaningful: keep.
	s := &Scheduler{
		cfg:   cfg,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
		space: make(chan struct{}),
		rng:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	s.applyRateLocked(cfg.RateBps, cfg.RTT)
	return s
}

// rateMTU resolves the serialisation basis: explicit MTU or Chunk.
func (s *Scheduler) rateMTU() uint32 {
	if s.cfg.MTU > 0 {
		return s.cfg.MTU
	}
	return uint32(s.cfg.Chunk)
}

// applyRateLocked recomputes quantum, AQM tuning, shaper clocks and buffer
// limit from a rate and RTT. Caller holds mu. Ports
// sch_cake.c:cake_set_rate + cake_reconfigure sizing.
func (s *Scheduler) applyRateLocked(rateBps uint64, rtt time.Duration) {
	if rtt <= 0 {
		rtt = DefaultRTT
	}
	rp := cake.ComputeRateParams(rateBps, s.rateMTU(), cake.DefaultTargetNs, int64(rtt))
	s.quantum = rp.FlowQuantum
	s.cparams = cake.CobaltParams{
		Interval: rp.IntervalNs,
		Target:   rp.TargetNs,
		MTUTime:  rp.MTUTimeNs,
		PInc:     rp.PInc,
		PDec:     rp.PDec,
	}
	// Unlimited keeps the previous AQM tuning when ComputeRateParams has
	// nothing to derive from (mtu==0 cannot happen here; rate==0 yields
	// quantum but zeroed target/interval only if mtu==0, which it isn't).
	// With rate==0 the shaper is disabled and the buffer stays at the
	// 4MiB floor: fairness without a ceiling.
	if rateBps == 0 {
		s.cparams = cake.CobaltParams{
			Interval: cake.DefaultIntervalNs,
			Target:   cake.DefaultTargetNs,
			MTUTime:  0,
			PInc:     1 << 24,
			PDec:     1 << 20,
		}
	}
	// Retuning must not forgive serialization already charged to admitted
	// bytes. Keep the clocks even across the temporary unlimited mode: it
	// ignores Wait while disabled, and stale clocks reset on idle enqueue.
	s.shaper.RateNs = rp.RateNs
	s.shaper.RateShft = rp.RateShft
	s.bufferLimit = cake.BufferLimit(rateBps, rp.IntervalNs)
	if s.bufferLimit == 0 {
		s.bufferLimit = cake.BufferLimit(0, 0)
	}
}

// SetRate updates the shaper rate and retunes quantum/AQM/buffer live.
// Pass BBR's SustainedBW (already margined by the caller) and MinRTT.
// Rate 0 disables shaping (fairness only). Safe for concurrent use.
func (s *Scheduler) SetRate(rateBps uint64, rtt time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyRateLocked(rateBps, rtt)
	s.notifySpaceLocked()
	s.signal()
}

// QueuedLen reports queued chunks (sparse + ring). Caller need not hold mu.
func (s *Scheduler) QueuedLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sparse) + len(s.ring)
}

// QueuedBytes reports queued payload bytes. Caller need not hold mu.
func (s *Scheduler) QueuedBytes() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bufferUsed
}

// Stats snapshots the counters. Caller need not hold mu.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		SparseTurns: s.sparseTurns,
		BulkTurns:   s.bulkTurns,
		Demoted:     s.demoted,
		TimedOut:    s.timedOut,
		Dropped:     s.dropped,
	}
}

func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// AddFlow registers a stream. The flow is usable until Finish or Close.
// New flows start with a full deficit quantum (first turn is immediate,
// sch_cake.c:cake_enqueue deficit init).
func (s *Scheduler) AddFlow(stream StreamWriter) *Flow {
	if st, ok := stream.(*smux.Stream); ok {
		stream = &smuxWriter{stream: st}
	}
	s.mu.Lock()
	q := s.quantum
	s.mu.Unlock()
	return &Flow{stream: stream, deficit: int32(q), freeCh: make(chan struct{}, 1), progress: make(chan struct{}, 1)}
}

// randU32 supplies the BLUE coin flip (sch_cake.c:get_random_u32).
func (s *Scheduler) randU32() uint32 {
	s.rngMu.Lock()
	defer s.rngMu.Unlock()
	return s.rng.Uint32()
}

// Submit hands one chunk to the scheduler and blocks until it has been
// written (or the flow/scheduler is closed). Called from the per-flow reader
// goroutine.
//
// The wait is bounded by SubmitTimeout, but the timer measures SCHEDULER
// STARVATION (never picked), not service time: every time the writer picks
// the chunk up yet has to requeue it (slow peer, full window), the timer
// resets. A saturated-but-moving pipe therefore never fails its flows,
// while a flow parked behind a storm still fails fast with ErrBusy instead
// of stalling indefinitely.
//
// Overlimit (sch_cake.c:cake_drop) also fails fast with ErrBusy: when total
// pending bytes exceed the buffer limit the longest bulk queue is dropped
// first, so a flood sacrifices a torrent peer rather than an interactive
// chunk; the victim's owner observes ErrBusy and tears the stream down,
// which end-to-end TCP repairs by retransmit.
func (s *Scheduler) Submit(f *Flow, chunk []byte) error {
	now := time.Now()
	s.mu.Lock()
	if s.closed || f.dead {
		s.mu.Unlock()
		return ErrFlowClosed
	}
	for s.cfg.Reliable && s.bufferUsed+uint64(len(chunk)) > s.bufferLimit {
		if uint64(len(chunk)) > s.bufferLimit {
			s.mu.Unlock()
			return ErrBusy
		}
		space := s.space
		s.mu.Unlock()
		select {
		case <-s.done:
			return ErrFlowClosed
		case <-space:
		}
		s.mu.Lock()
		if s.closed || f.dead {
			s.mu.Unlock()
			return ErrFlowClosed
		}
	}
	wasEmpty := len(s.sparse)+len(s.ring) == 0
	// Buffer overlimit before enqueue (sch_cake.c:cake_enqueue tail).
	if s.bufferUsed+uint64(len(chunk)) > s.bufferLimit {
		if s.dropLongestLocked(now, chunk) {
			// Victim dropped, room made: fall through and enqueue.
		} else {
			// Incoming is the longest (or nothing to drop): tail-drop it.
			s.mu.Unlock()
			return ErrBusy
		}
	}
	f.pending = chunk
	f.serving = false
	f.enqueueT = now
	f.ready = true
	sparse := f.lastSeen.IsZero() || now.Sub(f.lastSeen) > s.cfg.Idle
	f.lastSeen = now
	if !f.inQueue {
		f.inQueue = true
		if f.set == setNone && f.deficit <= 0 {
			// Re-entering flow that spent its deficit keeps it spent:
			// DRR byte-fairness lives in this persistence. Brand-new
			// flows already hold a full quantum from AddFlow.
			f.deficit += int32(s.quantum)
			if f.deficit <= 0 {
				f.deficit = int32(s.quantum)
			}
		}
		if sparse && s.cfg.MaxSparse > 0 && len(s.sparse) < s.cfg.MaxSparse {
			f.set = setSparse
			s.sparse = append(s.sparse, f)
		} else {
			if sparse {
				s.demoted++
			}
			f.set = setBulk
			s.ring = append(s.ring, f)
		}
	}
	s.bufferUsed += uint64(len(chunk))
	if wasEmpty {
		s.shaper.IdleReset(now)
	}
	s.mu.Unlock()
	s.signal()

	timer := time.NewTimer(s.cfg.SubmitTimeout)
	defer timer.Stop()
	var timeout <-chan time.Time
	if !s.cfg.Reliable {
		timeout = timer.C
	}
	for {
		select {
		case <-s.done:
			return ErrFlowClosed
		case <-f.freeCh:
			s.mu.Lock()
			dropBusy := f.dropBusy
			if dropBusy {
				f.dropBusy = false
			}
			dead := f.dead
			s.mu.Unlock()
			if dropBusy {
				return ErrBusy
			}
			if dead {
				return ErrFlowClosed
			}
			return nil
		case <-f.progress:
			// Served but slow: restart the starvation clock. Drain a
			// possibly-fired timer so Reset is race-free (single
			// goroutine owns this timer).
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.cfg.SubmitTimeout)
		case <-timeout:
			// Never picked in time: detach (if still queued) so the queue
			// cannot grow without bound, mark dead, fail fast.
			s.mu.Lock()
			if f.serving && !f.dead {
				s.mu.Unlock()
				timer.Reset(s.cfg.SubmitTimeout)
				continue
			}
			if !f.dead {
				f.dead = true
				if f.pending != nil {
					s.bufferUsed -= uint64(len(f.pending))
					s.notifySpaceLocked()
				}
				f.pending = nil
				f.ready = false
				s.removeLocked(f)
				s.timedOut++
			}
			s.mu.Unlock()
			return ErrBusy
		}
	}
}

// dropLongestLocked implements the overlimit drop (sch_cake.c:cake_drop):
// drop the longest bulk queue first (bulk flood pays, sparse is spared).
// Returns true if an already-queued victim was dropped to make room; false
// means the caller must tail-drop the incoming chunk. Caller holds mu.
func (s *Scheduler) dropLongestLocked(now time.Time, incoming []byte) bool {
	bestIdx := -1
	var bestLen int
	for i, f := range s.ring {
		if f.pending == nil {
			continue
		}
		if n := len(f.pending); n > bestLen {
			bestLen = n
			bestIdx = i
		}
	}
	if bestIdx >= 0 && bestLen >= len(incoming) {
		v := s.ring[bestIdx]
		s.ring = append(s.ring[:bestIdx], s.ring[bestIdx+1:]...)
		if s.rr > len(s.ring) {
			s.rr = 0
		}
		s.bufferUsed -= uint64(len(v.pending))
		s.notifySpaceLocked()
		v.pending = nil
		v.ready = false
		v.inQueue = false
		v.set = setNone
		v.dead = true
		v.dropBusy = true
		cake.CobaltQueueFull(&v.cvars, &s.cparams, now)
		s.dropped++
		select {
		case v.freeCh <- struct{}{}:
		default:
		}
		return true
	}
	// No bulk victim worth dropping (sparse-only backlog, or the incoming
	// chunk itself is the longest): tail-drop the incoming.
	return false
}

// Finish removes a flow (source closed or errored) and unblocks its reader.
// Idempotent: safe to call twice or after Close.
func (s *Scheduler) Finish(f *Flow) {
	s.mu.Lock()
	if f.pending != nil && f.inQueue {
		s.bufferUsed -= uint64(len(f.pending))
		s.notifySpaceLocked()
	}
	f.dead = true
	f.pending = nil
	f.ready = false
	s.removeLocked(f)
	s.notifySpaceLocked()
	s.mu.Unlock()
	select {
	case f.freeCh <- struct{}{}:
	default:
	}
}

func (s *Scheduler) notifySpaceLocked() {
	close(s.space)
	s.space = make(chan struct{})
}

// Close shuts the scheduler down: no new submits are accepted, every parked
// submitter is released (observing ErrFlowClosed), and the writer exits.
// Idempotent.
func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	// Release every parked submitter so no reader goroutine leaks: mark all
	// queued flows dead and wake them; they observe dead and return.
	for _, q := range [][]*Flow{s.sparse, s.ring} {
		for _, f := range q {
			f.dead = true
			f.pending = nil
			f.ready = false
			f.inQueue = false
			f.set = setNone
			select {
			case f.freeCh <- struct{}{}:
			default:
			}
		}
	}
	s.sparse = nil
	s.ring = nil
	s.bufferUsed = 0
	s.mu.Unlock()
	s.signal()
}

// removeLocked drops f from whichever queue holds it. Caller holds s.mu.
func (s *Scheduler) removeLocked(f *Flow) {
	if !f.inQueue {
		return
	}
	for i, g := range s.sparse {
		if g == f {
			s.sparse = append(s.sparse[:i], s.sparse[i+1:]...)
			f.inQueue = false
			f.set = setNone
			return
		}
	}
	for i, g := range s.ring {
		if g == f {
			s.ring = append(s.ring[:i], s.ring[i+1:]...)
			if s.rr > len(s.ring) {
				s.rr = 0
			}
			f.inQueue = false
			f.set = setNone
			return
		}
	}
}

// nextLocked picks the next flow to service and detaches its pending chunk.
// Sparse goes first, but at most MaxBurst sparse turns run back-to-back
// before one ring turn is forced, so bulk always makes progress even under a
// storm of fresh connections. Within each list the DRR++ deficit applies
// (sch_cake.c:cake_dequeue): a flow with exhausted deficit is topped up and
// rotated to the ring tail unserved; a sparse flow that exhausts its deficit
// while still holding data demotes to bulk, exactly as CAKE moves
// CAKE_SET_SPARSE to CAKE_SET_BULK. Caller holds s.mu. Returns nil if none
// ready.
func (s *Scheduler) nextLocked() (*Flow, []byte) {
	for {
		var f *Flow
		var fromSparse bool
		if len(s.sparse) > 0 && (len(s.ring) == 0 || s.cfg.MaxBurst <= 0 || s.consecSparse < s.cfg.MaxBurst) {
			f = s.sparse[0]
			fromSparse = true
		} else if len(s.ring) > 0 {
			if s.rr >= len(s.ring) {
				s.rr = 0
			}
			f = s.ring[s.rr]
		} else {
			return nil, nil
		}
		if f.deficit <= 0 {
			// Deficit exhausted: top up and rotate unserved.
			if fromSparse {
				s.sparse = s.sparse[1:]
				f.set = setBulk
				f.deficit += int32(s.quantum)
				s.ring = append(s.ring, f)
			} else {
				s.ring = append(s.ring[:s.rr], s.ring[s.rr+1:]...)
				if s.rr >= len(s.ring) {
					s.rr = 0
				}
				f.deficit += int32(s.quantum)
				s.ring = append(s.ring, f)
			}
			continue
		}
		// Serve f: detach from its list.
		if fromSparse {
			s.sparse = s.sparse[1:]
			s.consecSparse++
			s.sparseTurns++
		} else {
			s.ring = append(s.ring[:s.rr], s.ring[s.rr+1:]...)
			if s.rr >= len(s.ring) {
				s.rr = 0
			}
			s.consecSparse = 0
			s.bulkTurns++
		}
		f.inQueue = false
		f.set = setNone
		chunk := f.pending
		f.pending = nil
		f.ready = false
		if chunk != nil {
			f.deficit -= int32(len(chunk))
			s.bufferUsed -= uint64(len(chunk))
			s.notifySpaceLocked()
			// Cobalt dequeue AQM (sch_cake.c:cobalt_should_drop):
			// evaluated for state evolution, but at the tunnel's
			// depth of one chunk per flow the kernel's last-packet
			// rule always delivers — never silently dropped here.
			// Overlimit and starvation are the dropping signals.
			now := time.Now()
			_, _ = cake.CobaltShouldDrop(&f.cvars, &s.cparams, now, f.enqueueT, len(s.ring), s.randU32())
		}
		return f, chunk
	}
}

// Run is the single writer loop. Run it as go s.Run(); it exits on Close.
func (s *Scheduler) Run() {
	for {
		s.mu.Lock()
		for !s.closed && len(s.sparse) == 0 && len(s.ring) == 0 {
			s.mu.Unlock()
			<-s.wake
			s.mu.Lock()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		// Deficit-mode shaper gate (sch_cake.c:cake_dequeue): while the
		// shaper lies in the future, wait for it. Wakeups re-evaluate
		// (Close must interrupt the sleep).
		if w := s.shaper.Wait(time.Now()); w > 0 {
			s.mu.Unlock()
			t := time.NewTimer(w)
			select {
			case <-s.wake:
				if !t.Stop() {
					select {
					case <-t.C:
					default:
					}
				}
				continue
			case <-t.C:
				continue
			}
		}
		f, chunk := s.nextLocked()
		if f != nil && chunk != nil {
			f.serving = true
			s.shaper.Advance(uint32(len(chunk)), time.Now(), false)
		}
		s.mu.Unlock()
		if f == nil {
			continue
		}

		// Write outside the lock so a blocked write can't stall bookkeeping.
		_ = f.stream.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		n, err := f.stream.Write(chunk)
		_ = f.stream.SetWriteDeadline(time.Time{})
		if n > 0 {
			f.lastServe.Store(time.Now().UnixNano())
		}

		if n < len(chunk) && (isTimeout(err) || (err == nil && n > 0)) {
			// Slow peer / full window, or a short write with no error
			// (legal for an io.Writer, rare for smux): the tail was
			// detached from the flow, so requeue the remainder at the
			// ring tail (never sparse — it has already had a turn) and
			// keep the reader parked. Do NOT release freeCh. Signal
			// progress so the reader's starvation timer resets: the
			// writer is provably serving this flow, slowly is not
			// starvation (same contract as the timeout path).
			s.mu.Lock()
			f.serving = false
			if !f.dead {
				f.pending = chunk[n:]
				f.ready = true
				f.set = setBulk
				if !f.inQueue {
					f.inQueue = true
					s.ring = append(s.ring, f)
				}
				s.bufferUsed += uint64(len(f.pending))
				select {
				case f.progress <- struct{}{}:
				default:
				}
			}
			s.mu.Unlock()
			continue
		}
		if n == 0 && err == nil && len(chunk) > 0 {
			// Zero progress with no error, forever, would spin the
			// single writer loop and starve every flow (requeue makes
			// no progress by definition). No real writer does this;
			// fail this flow fast so the blast radius stays at one
			// flow and the app retries.
			s.Finish(f)
			continue
		}
		if err != nil && !isTimeout(err) {
			// Fatal: drop the flow and let the reader tear down.
			// Finish unblocks the parked reader.
			s.Finish(f)
			continue
		}
		// Chunk fully written: let the reader fetch the next one.
		select {
		case f.freeCh <- struct{}{}:
		default:
		}
	}
}

// Linger waits, after a clean pump end, until the flow has been quiet for
// Config.Linger before the caller sends FIN (closes the stream).
//
// Why: smux orders its send queue by class with control ahead of data, so a
// FIN sent while the flow's last data frames are still queued jumps ahead
// of them. The peer then sees EOF, stops reading, and the tail bytes die
// unreported (exact-size reads come up short with no error anywhere). Flows
// idle longer than Linger already drained, so only hot flows wait, and only
// for the remainder. Call it only on the clean path (pump returned the
// source EOF); on errors close immediately, the flow is broken anyway.
// Linger <= 0 disables the wait.
func (s *Scheduler) Linger(f *Flow) {
	w := s.cfg.Linger
	if w <= 0 {
		return
	}
	if last := f.lastServe.Load(); last != 0 {
		if rest := time.Until(time.Unix(0, last).Add(w)); rest > 0 {
			time.Sleep(rest)
		}
	}
}
func isTimeout(err error) bool {
	// smux's own ErrTimeout is a plain sentinel (no Timeout() method), so
	// match it explicitly. Real smux streams are wrapped by smuxWriter so
	// this means our scheduler wait expired, not an ambiguous smux write.
	if errors.Is(err, smux.ErrTimeout) {
		return true
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// Pump reads from src in bounded chunks and feeds them to the scheduler for
// f, applying backpressure (Submit blocks until each chunk is written).
// Returns when src is exhausted (io.EOF), the flow is torn down
// (ErrFlowClosed), or the scheduler is saturated (ErrBusy — tear down fast,
// let the app retry).
func (s *Scheduler) Pump(f *Flow, src io.Reader) error {
	buf := make([]byte, s.cfg.Chunk)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if serr := s.Submit(f, chunk); serr != nil {
				return serr
			}
		}
		if err != nil {
			return err
		}
	}
}
