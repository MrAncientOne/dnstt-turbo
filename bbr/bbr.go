// Package bbr implements a BBR-inspired congestion controller for a single
// DNS-tunnel session (BBR-lite).
//
// It keeps the model, not the machinery: continuously estimate bottleneck
// bandwidth as windowed-maximum delivery rate and RTT as windowed-minimum,
// size in-flight targets from their product and a configurable gain, and
// pace emission through startup, probing and drain phases. What it deliberately omits versus full BBR: loss
// recovery (the TCP substrate and KCP handle it), ECN, coexistence and
// fairness logic (the tunnel is one flow that wants everything it can
// take). References: Cardwell et al., "BBR: Congestion-Based Congestion
// Control" (CACM 2017); draft-ietf-ccwg-bbr / linux tcp_bbr.c for gains
// (startup 2.77, drain 0.5, ProbeBW cycle 1.25/0.9/1.0, ProbeRTT 200ms per
// 5s); quic-go and x/net pacer designs for the token bucket.
//
// Defaults, not destiny: seeds come from static config (later the
// auto-tuner), but every estimate must converge away from a wrong seed by
// measurement. Only the hard clamp range is fixed.
//
// All time comes from Config.Now so tests drive the controller
// deterministically; production passes time.Now.
package bbr

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Gains follow the BBR literature.
const (
	startupGain   = 2.77 // doubling each round (4*ln2)
	drainGain     = 0.5  // inverse of startup cwnd gain 2
	probeUpGain   = 1.25 // probe for more bandwidth
	probeDownGain = 0.9  // drain queue / yield
	cruiseGain    = 1.0  // ride the estimate
	pacingMargin  = 0.99 // aim 1% under, per bbr_pacing_margin_percent

	// maxPacerWait caps any single send's pacing delay. Without it, a
	// collapsed rate estimate digs unbounded bucket debt and wedges
	// traffic for minutes (live deadlock: queries sleeping past any
	// recovery). Late is always better than never.
	maxPacerWait = 2 * time.Second

	fullBWThresh = 1.25 // growth below this for 3 rounds = pipe full
	fullBWRounds = 3

	// maxBWWindowLen is the wall-time horizon of the windowed-max delivery
	// filter. BBRv1 uses ~10 packet-timed round trips; on a DNS tunnel
	// with 80ms RTT that is ~800ms, far too twitchy for 100ms ticks over
	// a poll-driven KCP clock. 8s holds through momentary gaps and idle
	// (app-limited samples only enter when they raise the estimate, and an empty window
	// keeps the old estimate instead of resyncing to the current tick),
	// while still tracking a sustained degradation within seconds.
	// Live lesson: instant downward resync turned a pacer-induced dip
	// into a permanent 1KB/s latch. Never do that.
	maxBWWindowLen = 8 * time.Second

	probeRTTInterval = 5 * time.Second
	probeRTTDuration = 200 * time.Millisecond
	minRTTWindow     = 10 * time.Second

	// minRTTFloor rejects scheduling artifacts below any physical
	// DNS-over-TCP path. Live failure: unmatched query/response timing
	// produced 2-5ms samples on an 80ms path, collapsing BDP to bytes
	// and pinning the window to the floor. Samples below the floor are
	// discarded, never admitted to the min filter.
	minRTTFloor = 10 * time.Millisecond
	// minRTTCeil rejects queue-inflated outliers above any plausible
	// propagation delay on this path. Live failure: FIFO-matched bulk
	// responses under a stream flood measured queue position (10s+) on
	// an 84ms path; admitting them exploded BDP and pinned the window
	// at max. True RTprop here is tens of ms; anything past the ceiling
	// is queueing or retransmit delay, never propagation.
	minRTTCeil = 2 * time.Second

	// Legacy package default; callers can choose a lower data probing floor.
	// DNS polls and control-only packets bypass payload pacing.
	minPacingRate = 32 * 1024.0 // B/s

	// maxRoundLen bounds packet-timed rounds in wall time. Rounds advance
	// when cumulative delivery passes one BDP; if delivery stalls
	// entirely the state machine must still tick (slowly) rather than
	// freeze forever.
	maxRoundLen = 1 * time.Second
)

// State is the controller mode.
type State int

const (
	StateStartup State = iota
	StateDrain
	StateProbeBW
	StateProbeRTT
)

// String names states for logs.
func (s State) String() string {
	switch s {
	case StateStartup:
		return "startup"
	case StateDrain:
		return "drain"
	case StateProbeBW:
		return "probebw"
	case StateProbeRTT:
		return "probertt"
	}
	return "unknown"
}

// Config tunes a Controller. Zero values take documented defaults;
// MinSegs/MaxSegs clamp the window and are the only fixed rails.
//
// SegBytes is the downstream-effective bytes per KCP segment used for
// BDP sizing (default 1200, the KCP segment scale responses bundle at),
// NOT the upstream per-query MTU. Passing the upstream MTU (~500 or the
// QNAME ~100) understates the window by an order of magnitude.
//
// MinSegs and MaxSegs are independent window rails. Production uses one
// Controller per direction; SegBytes/UpSegBytes convert the same model into
// segment units for callers that require both accessors.
type Config struct {
	// WindowGain reserves flight headroom for probing and ACK scheduling.
	// MaxFlightBytes is a per-direction memory/flight budget, independent of
	// the minimum window. Zero values retain legacy gain/rate defaults.
	WindowGain float64
	// MinFlightTime reserves enough flight for a transport's batching cadence.
	// It changes window sizing, not RTT measurement or the bandwidth model.
	MinFlightTime  time.Duration
	MaxFlightBytes uint64
	MinPacingBps   float64
	SeedBWBps      float64       // initial bandwidth estimate (B/s)
	SeedRTT        time.Duration // initial RTT estimate
	MinSegs        int           // floor for window (default 64)
	MaxSegs        int           // ceiling for window (default 1024)
	SegBytes       int           // downstream-effective bytes per segment (default 1200)
	UpSegBytes     int           // upstream-effective bytes per segment (default 500)
	Now            func() time.Time
}

func (c *Config) withDefaults() Config {
	o := *c
	if o.MinFlightTime < 0 {
		o.MinFlightTime = 0
	}
	if o.WindowGain < 1 || math.IsNaN(o.WindowGain) || math.IsInf(o.WindowGain, 0) {
		o.WindowGain = 1
	}
	if o.MaxFlightBytes == 0 {
		o.MaxFlightBytes = 64 << 20
	}
	if o.MinPacingBps <= 0 || math.IsNaN(o.MinPacingBps) || math.IsInf(o.MinPacingBps, 0) {
		o.MinPacingBps = minPacingRate
	}
	if o.SeedBWBps <= 0 || math.IsNaN(o.SeedBWBps) || math.IsInf(o.SeedBWBps, 0) {
		o.SeedBWBps = 2e6 / 8 // 2 Mbps: low seed, startup searches up fast
	}
	if o.SeedRTT <= 0 || o.SeedRTT > minRTTCeil {
		// A seed above the physical ceiling would pin the window at
		// max for a full min-RTT window with no sample able to pull it
		// down faster than the damped expiry; clamp into the band.
		o.SeedRTT = 100 * time.Millisecond
	}
	if o.MinSegs <= 0 {
		o.MinSegs = 64
	}
	if o.MaxSegs <= 0 {
		o.MaxSegs = 1024
	}
	if o.MaxSegs < o.MinSegs {
		// Inverted rails would pin the window at MaxSegs below the
		// proven-static floor; the floor wins, the ceiling follows it.
		o.MaxSegs = o.MinSegs
	}
	if o.MaxSegs > MaxWindowSegments {
		o.MaxSegs = MaxWindowSegments
	}
	if o.MinSegs > o.MaxSegs {
		o.MinSegs = o.MaxSegs
	}
	if o.SegBytes <= 0 {
		o.SegBytes = 1200
	}
	if o.UpSegBytes <= 0 {
		o.UpSegBytes = 500
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Sample is one measurement interval (nominally ~100ms of traffic).
type Sample struct {
	DeliveredBytes uint64        // downstream payload bytes delivered this interval
	SentBytes      uint64        // upstream wire bytes sent this interval (for send-rate cap)
	Interval       time.Duration // wall length of the interval
	AppLimited     bool          // sender had nothing to send: rate understates
	RTT            time.Duration // fresh matched RTT observation, if any
	HasRTT         bool
}

// bwPoint is one entry of the windowed-max delivery filter.
type bwPoint struct {
	t  time.Time
	bw float64
}

// Controller is a BBR-lite control loop for one direction of one session.
// It is not safe for concurrent use; the owner serializes Update calls
// (e.g. one goroutine ticking at 100ms).
type Controller struct {
	cfg Config
	now func() time.Time

	state State

	maxBW       float64 // windowed-max delivery rate (B/s)
	maxBWpts    []bwPoint
	minRTT      time.Duration
	minRTTAt    time.Time
	measuredRTT bool

	probeRTTAt time.Time // last ProbeRTT entry (rate-limited)

	fullBW      float64
	fullBWCount int
	rounds      int // packet-timed rounds observed
	roundStart  time.Time
	// nextRoundDelivered is the cumulative-delivered threshold ending the
	// current round (packet-timed round counting per the BBR spec: a round
	// ends when delivery passes one BDP, not when wall time passes one
	// estimated RTT).
	nextRoundDelivered uint64

	seeded bool // true until the first real (non-app-limited) sample

	gain        float64 // current pacing gain
	probeRounds int     // position in the ProbeBW gain cycle (cycle use only)

	pacingRate float64 // B/s, what the pacer enforces
	windowSegs int     // downstream segments, what SetWindowSize takes as rcvwnd
	sendSegs   int     // upstream segments, what SetWindowSize takes as sndwnd

	lastUpdate   time.Time
	lastDelivery time.Time
	delivered    uint64 // lifetime delivered bytes (observability + round clock)
}

// NewController builds a Controller in Startup, paced and windowed from seeds.
func NewController(c Config) *Controller {
	cfg := c.withDefaults()
	t0 := cfg.Now()
	// Initial round size is one seed-BDP so the first round ends after a
	// full flight, not after one tick.
	bdpBytes := uint64(cfg.SeedBWBps * cfg.SeedRTT.Seconds())
	if bdpBytes < uint64(4*cfg.SegBytes) {
		bdpBytes = uint64(4 * cfg.SegBytes)
	}
	ctl := &Controller{
		cfg:                cfg,
		now:                cfg.Now,
		state:              StateStartup,
		maxBW:              cfg.SeedBWBps,
		minRTT:             cfg.SeedRTT,
		minRTTAt:           t0,
		gain:               startupGain,
		lastUpdate:         t0,
		lastDelivery:       t0,
		roundStart:         t0,
		nextRoundDelivered: bdpBytes, // delivered starts at 0
		seeded:             true,
	}
	ctl.recompute()
	return ctl
}

// maxBWWindow prunes points older than maxBWWindowLen and returns the max.
// An empty window returns 0; the caller keeps the old estimate (never
// resyncs to the current tick). Holding through gaps is the specified
// behavior: app-limited samples only enter when they raise the estimate, and a quiet or
// briefly stalled path must not zero the model.
func (c *Controller) maxBWWindow(now time.Time) float64 {
	keep := c.maxBWpts[:0]
	best := 0.0
	for _, p := range c.maxBWpts {
		if now.Sub(p.t) <= maxBWWindowLen {
			keep = append(keep, p)
			if p.bw > best {
				best = p.bw
			}
		}
	}
	c.maxBWpts = keep
	return best
}

// SustainedBW returns the median delivery rate over the same window the
// max filter spans: the BBRv2 long_term idea (a companion to the max for
// longer-horizon decisions) with a robust statistic. The max must drive
// pacing and window (spec: probe headroom lives in burst peaks), but
// anything targeting a longer horizon, like a shaper rate, must use the
// sustained figure: burst peaks overread single-flow reality 2-3x, and
// targeting them feeds a queue. Median, not mean: one giant tick (a queue
// draining at 10x link rate into a single 100ms sample) moves a windowed
// mean several-fold but cannot move a median at all, while a genuine
// step change still converts the median within half a window.
// App-limited ticks never enter the points, so idle never drags it; an
// empty window returns the current max (hold, never zero).
func (c *Controller) SustainedBW() float64 {
	now := c.now()
	keep := c.maxBWpts[:0]
	rates := make([]float64, 0, len(c.maxBWpts))
	for _, p := range c.maxBWpts {
		if now.Sub(p.t) <= maxBWWindowLen {
			keep = append(keep, p)
			rates = append(rates, p.bw)
		}
	}
	c.maxBWpts = keep
	if len(rates) == 0 {
		return c.maxBW
	}
	sort.Float64s(rates)
	mid := len(rates) / 2
	if len(rates)%2 == 1 {
		return rates[mid]
	}
	return (rates[mid-1] + rates[mid]) / 2
}

// rtt returns the effective RTT estimate, floored (never zero, never a
// scheduling artifact).
func (c *Controller) rtt() time.Duration {
	if c.minRTT < minRTTFloor {
		return minRTTFloor
	}
	return c.minRTT
}

// bdpBytes returns the current BDP in bytes, clamped to a sane flight.
func (c *Controller) bdpBytes() uint64 {
	bdp := c.maxBW * c.rtt().Seconds()
	if bdp < float64(4*c.cfg.SegBytes) {
		bdp = float64(4 * c.cfg.SegBytes)
	}
	if bdp > float64(c.cfg.MaxFlightBytes) {
		bdp = float64(c.cfg.MaxFlightBytes)
	}
	return uint64(bdp)
}

// Update folds one Sample into the estimators and advances the state
// machine. Call about every 100ms with that interval's traffic.
func (c *Controller) Update(s Sample) {
	now := c.now()
	if s.Interval <= 0 {
		return
	}
	if s.DeliveredBytes > 0 {
		if now.Sub(c.lastDelivery) > maxBWWindowLen && !s.AppLimited {
			// Capacity can change during an outage. Re-enter discovery instead
			// of taking many slow probe cycles to escape the recovery floor.
			c.state = StateStartup
			c.seeded = true
			c.fullBW = 0
			c.fullBWCount = 0
			c.roundStart = now
			c.nextRoundDelivered = c.delivered + c.bdpBytes()
		}
		c.lastDelivery = now
	}
	rawRate := float64(s.DeliveredBytes) / s.Interval.Seconds()
	// Feedback supplies unique payload delivery per direction. The receiver
	// has no remote send timestamp, so this remains an interval-rate model,
	// not full BBR delivery-rate sampling; ACK compression can bias peaks.
	rate := rawRate
	c.delivered += s.DeliveredBytes

	// RTT: windowed minimum over minRTTWindow. Samples outside the
	// physical band are queueing/scheduling outliers, never propagation:
	// drop them. On expiry the rise is damped to 2x per episode: a stale
	// filter must re-measure upward gradually, never jump 84ms to seconds
	// on one queue-inflated sample (live window pinned at max).
	if s.HasRTT && s.RTT >= minRTTFloor && s.RTT <= minRTTCeil {
		if !c.measuredRTT || s.RTT < c.minRTT {
			c.measuredRTT = true
			c.minRTT = s.RTT
			c.minRTTAt = now
		} else if now.Sub(c.minRTTAt) > minRTTWindow {
			c.minRTT *= 2
			if s.RTT < c.minRTT {
				c.minRTT = s.RTT
			}
			c.minRTTAt = now
		}
	}
	// Guard against a poisoned minRTT planted before the floor existed
	// (or via seed): clamp up to the floor, keep the timestamp so the
	// window can still expire normally.
	if c.minRTT < minRTTFloor {
		c.minRTT = minRTTFloor
	}

	// Bandwidth: windowed maximum; limited samples may raise, never lower it
	// (they understate the path). The seed evaporates on the first real
	// sample so a wrong seed can never pin the estimate: convergence is
	// by measurement only. An empty window keeps the old estimate: a
	// stall or idle gap must age out over the horizon, never collapse
	// instantly to the current tick.
	// A feedback gap has no delivery-rate measurement. In particular, it
	// must not evaporate the startup seed into zero before the outage
	// horizon. Sustained zero delivery still backs off below.
	if rate > 0 && (!s.AppLimited || rate > c.maxBW) {
		if c.seeded {
			c.seeded = false
			c.maxBWpts = nil
			c.maxBW = 0
		}
		c.maxBWpts = append(c.maxBWpts, bwPoint{now, rate})
	}
	if m := c.maxBWWindow(now); m > 0 {
		c.maxBW = m
	} else if !s.AppLimited && now.Sub(c.lastDelivery) >= maxBWWindowLen {
		// A sustained outage is not idle. Back off while retaining enough
		// pacing for a recovery probe; pure control packets bypass this rate.
		c.maxBW = c.cfg.MinPacingBps
	}
	// else: hold c.maxBW (deliberate; see above).

	// Packet-timed round counting per the spec: a round ends when
	// cumulative delivery passes one BDP flight. Wall-time fallback
	// (maxRoundLen) keeps the machine ticking through total stalls.
	roundStart := false
	if c.delivered >= c.nextRoundDelivered {
		roundStart = true
		c.rounds++
		c.roundStart = now
		c.nextRoundDelivered = c.delivered + c.bdpBytes()
	} else if now.Sub(c.roundStart) >= maxRoundLen {
		roundStart = true
		c.rounds++
		c.roundStart = now
		// Do not advance the delivered threshold on a time-only round:
		// the next real flight still ends the packet round.
	}

	switch c.state {
	case StateStartup:
		c.gain = startupGain
		// Full-pipe estimator: 3 non-app-limited packet rounds under 25% growth.
		// A zero-rate round counts as no growth even with fullBW still
		// unset: otherwise a stall from boot resets the counter every
		// round and Startup holds its doubling gain forever, salting
		// the queue that the first recovery then suffers through.
		if roundStart && !s.AppLimited {
			if rate > 0 && (c.fullBW == 0 || rate >= c.fullBW*fullBWThresh) {
				c.fullBW = math.Max(c.fullBW, rate)
				c.fullBWCount = 0
			} else {
				c.fullBWCount++
				if c.fullBWCount >= fullBWRounds {
					c.state = StateDrain
					c.gain = drainGain
					c.fullBWCount = 0
				}
			}
		}
	case StateDrain:
		c.gain = drainGain
		// Exit when delivery falls back to the estimate (queue drained).
		if roundStart && rate <= c.maxBW*1.1 {
			c.state = StateProbeBW
			c.gain = cruiseGain
			c.probeRounds = 0
		}
	case StateProbeBW:
		c.probeCycle(roundStart)
	case StateProbeRTT:
		// Hold reduced inflight for the duration, then resume probing.
		if now.Sub(c.probeRTTAt) >= probeRTTDuration {
			c.state = StateProbeBW
			c.gain = cruiseGain
			c.probeRounds = 0
		}
	}

	// ProbeRTT scheduling: stale min-RTT with continuous sending.
	if c.state != StateProbeRTT && now.Sub(c.minRTTAt) > minRTTWindow &&
		now.Sub(c.probeRTTAt) > probeRTTInterval && !s.AppLimited {
		c.state = StateProbeRTT
		c.probeRTTAt = now
	}

	c.lastUpdate = now
	c.recompute()
}

// ExitStartupEarly bails out of Startup into Drain on an external
// congestion signal (BBRv3 exits Startup on loss/ECN past target; we have
// no per-packet loss accounting over the tunnel, so the caller feeds the
// profiler's degraded verdict instead). Without this, Startup's doubling
// gain keeps flooding an already-collapsing path until the full-pipe
// estimator notices, salting the middlebox queue that later reps then
// suffer through. No-op unless currently in Startup.
func (c *Controller) ExitStartupEarly() {
	if c.state != StateStartup {
		return
	}
	c.state = StateDrain
	c.gain = drainGain
	c.fullBWCount = 0
}

// probeCycle walks cruise → up → down across packet-timed rounds.
func (c *Controller) probeCycle(roundStart bool) {
	if !roundStart {
		return
	}
	c.probeRounds++
	switch {
	case c.probeRounds == 1:
		c.gain = probeUpGain // one round faster to find headroom
	case c.probeRounds == 2:
		c.gain = probeDownGain // one round slower to drain the probe queue
	default:
		c.gain = cruiseGain // cruise until the next cycle
		if c.probeRounds >= 8 {
			c.probeRounds = 0
		}
	}
}

// recompute derives pacing rate and both windows from the model.
func (c *Controller) recompute() {
	bw := c.maxBW
	c.pacingRate = bw * c.gain * pacingMargin
	if c.pacingRate < c.cfg.MinPacingBps {
		c.pacingRate = c.cfg.MinPacingBps
	}

	// Reserve configured BDP headroom, then enforce both segment and byte rails.
	flightTime := max(c.rtt(), c.cfg.MinFlightTime)
	bdp := math.Min(bw*flightTime.Seconds()*c.cfg.WindowGain, float64(c.cfg.MaxFlightBytes))
	segs := int(math.Ceil(bdp / float64(c.cfg.SegBytes)))
	usegs := int(math.Ceil(bdp / float64(c.cfg.UpSegBytes)))
	if c.state == StateProbeRTT {
		// Halve inflight to drain and re-measure (spec cwnd_gain 0.5),
		// but never below the floor: the session must carry polls.
		segs = segs / 2
		usegs = usegs / 2
	}
	if segs < c.cfg.MinSegs {
		segs = c.cfg.MinSegs
	}
	if segs > c.cfg.MaxSegs {
		segs = c.cfg.MaxSegs
	}
	if usegs < c.cfg.MinSegs {
		usegs = c.cfg.MinSegs
	}
	if usegs > c.cfg.MaxSegs {
		usegs = c.cfg.MaxSegs
	}
	// The byte budget wins over the window floor if a caller supplies
	// contradictory settings. CLI configuration rejects that combination.
	segs = capFlight(segs, c.cfg.SegBytes, c.cfg.MaxFlightBytes)
	usegs = capFlight(usegs, c.cfg.UpSegBytes, c.cfg.MaxFlightBytes)
	c.windowSegs = segs
	c.sendSegs = usegs
}

func capFlight(segs, size int, budget uint64) int {
	cap := int(budget / uint64(size))
	if cap < 1 {
		cap = 1
	}
	if segs > cap {
		return cap
	}
	return segs
}

// SetSegmentBytes updates window units when the peer's segment size is learned.
func (c *Controller) SetSegmentBytes(n int) {
	if n > 0 {
		c.cfg.SegBytes = n
		c.cfg.UpSegBytes = n
		c.recompute()
	}
}

// PacingRate returns bytes/second the sender should pace at.
func (c *Controller) PacingRate() float64 { return c.pacingRate }

// WindowSegments returns the KCP receive window to enforce.
func (c *Controller) WindowSegments() int { return c.windowSegs }

// SendWindowSegments returns the KCP send window to enforce: the same BDP
// in upstream-sized segments, subject to both configured bounds.
func (c *Controller) SendWindowSegments() int { return c.sendSegs }

// State returns the controller mode (logs/tests).
func (c *Controller) State() State { return c.state }

// MaxBW returns the current bandwidth estimate (B/s).
func (c *Controller) MaxBW() float64 { return c.maxBW }

// MinRTT returns the current RTT estimate.
func (c *Controller) MinRTT() time.Duration { return c.minRTT }

// Delivered returns lifetime delivered bytes.
func (c *Controller) Delivered() uint64 { return c.delivered }

// Policed reports whether the policer-cap model is engaged.
// Deprecated: the plateau-duration policer latch caused the live 1KB/s
// deadlock (a hard cap is just a lower BtlBw; the windowed max converges
// there without a separate model, and without loss/ECN signals a plateau
// is expected, not a policer). Kept for log compatibility; always false.
func (c *Controller) Policed() bool { return false }

// Pacer is a token bucket that smooths emission at a measured rate,
// following quic-go/x-net practice: byte bucket, capped burst so
// sub-millisecond spacings never ask a timer to do the impossible,
// burst allowance after idle so interactive traffic never waits.
//
// Safe for concurrent use: in production SetRate runs on the estimator
// ticker goroutine while Wait/Advance run on the send goroutine.
type Pacer struct {
	mu      sync.Mutex
	rateBps float64
	burst   float64 // max bucket, bytes
	bucket  float64
	primed  bool
	last    time.Time
	now     func() time.Time
}

// NewPacer builds a bucket primed full (idle start sends immediately,
// the burst allowance after quiescence both quic-go and the IETF draft
// prescribe).
func NewPacer(now func() time.Time) *Pacer {
	if now == nil {
		now = time.Now
	}
	return &Pacer{now: now, last: now(), primed: false}
}

// SetRate updates the fill rate (B/s). Non-positive pauses pacing decisions
// (Wait returns zero: caller sends immediately). The first call primes the
// bucket full: idle flows send immediately up to the burst allowance.
func (p *Pacer) SetRate(bps float64, burstBytes float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rateBps = bps
	if burstBytes < 1 {
		burstBytes = 1
	}
	p.burst = burstBytes
	if !p.primed {
		p.primed = true
		p.bucket = p.burst
	} else if p.bucket > p.burst {
		p.bucket = p.burst
	}
}

// Wait blocks until n bytes may be sent at the configured rate, then
// consumes them. Zero rate or empty bucket-full returns immediately.
func (p *Pacer) Wait(n int) {
	if d := p.Advance(n); d > 0 {
		time.Sleep(d)
	}
}

// Advance records a send of n bytes and returns how long the caller should
// have waited first (zero = send now). Pure math, no sleeping: tests and
// callers that manage their own timers use this. The wait is capped at
// maxPacerWait and the bucket floored at the same horizon, so no single
// send can wedge traffic no matter how far the estimate collapses.
func (p *Pacer) Advance(n int) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rateBps <= 0 {
		return 0
	}
	now := p.now()
	el := now.Sub(p.last).Seconds()
	if el < 0 {
		el = 0
	}
	p.last = now
	p.bucket += el * p.rateBps
	if p.bucket > p.burst {
		p.bucket = p.burst
	}
	p.bucket -= float64(n)
	if p.bucket >= 0 {
		return 0
	}
	// Owed time to refill the deficit, bounded: floor the bucket so the
	// debt stays within one maxPacerWait horizon.
	floor := -p.rateBps * maxPacerWait.Seconds()
	if p.bucket < floor {
		p.bucket = floor
	}
	d := time.Duration((-p.bucket / p.rateBps) * float64(time.Second))
	if d > maxPacerWait {
		d = maxPacerWait
	}
	return d
}
