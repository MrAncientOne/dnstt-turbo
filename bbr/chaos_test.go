package bbr

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

// This file simulates the real world so the tunnel does not have to.
// Fixed-link unit tests in bbr_test.go prove the math; here randomized
// plants prove survival: jittered RTT, loss, token-bucket policers,
// bandwidth steps, idle gaps, and the 2ms scheduling-artifact RTT that
// killed production (must be floored, never admitted).
//
// All randomness is seeded for repro: a failing subtest logs its seed and
// parameters, and rerunning with that seed reproduces the exact stream.

// chaosPlant is a time-varying link with jitter, loss, and an optional
// token-bucket policer sitting in front of it.
type chaosPlant struct {
	rng      *rand.Rand
	linkBps  float64
	baseRTT  time.Duration
	jitter   time.Duration // uniform +/- per-sample noise
	lossFrac float64       // fraction of each tick's delivery dropped

	policerBps float64 // 0 = no policer
	burstBytes float64
	bucket     float64

	// spoofProb injects a 1-3ms RTT sample (the old unmatched-timing bug)
	// to prove the floor rejects it.
	spoofProb float64
}

func (p *chaosPlant) capBps() float64 {
	if p.policerBps > 0 && p.policerBps < p.linkBps {
		return p.policerBps
	}
	return p.linkBps
}

// tick simulates dt of backlogged transfer at the controller's pacing rate.
// It returns delivered payload bytes and a matched RTT sample.
//
// Policer model: sustained rate policerBps with a burst reserve. Demand up
// to the sustained rate always passes; excess draws from the burst reserve,
// which refills from unused sustained capacity. This sustains the policer
// rate indefinitely (unlike a naive per-tick bucket cap, which would cap
// sustained throughput to burst/dt whenever burst < rate*dt).
func (p *chaosPlant) tick(demandBps float64, dt time.Duration) (uint64, time.Duration, bool) {
	want := demandBps * dt.Seconds()
	allow := p.linkBps * dt.Seconds()
	if p.policerBps > 0 {
		sustained := p.policerBps * dt.Seconds()
		if want <= sustained {
			allow = math.Min(want, allow)
			p.bucket += sustained - want // unused capacity refills burst
			if p.bucket > p.burstBytes {
				p.bucket = p.burstBytes
			}
		} else {
			fromBurst := math.Min(p.bucket, want-sustained)
			p.bucket -= fromBurst
			allow = math.Min(allow, sustained+fromBurst)
		}
	}
	got := math.Min(want, allow)
	// Loss drops a fraction of what the link would deliver.
	if p.lossFrac > 0 && got > 0 {
		got *= (1 - p.lossFrac)
		// Randomize loss per tick around the mean so tests see bursts too.
		got *= 0.8 + 0.4*p.rng.Float64()
	}
	// RTT: base + queue delay when probing past capacity + jitter.
	rtt := p.baseRTT
	if want > allow*1.1 {
		rtt += 30 * time.Millisecond // queue stood up behind the probe
	}
	if p.jitter > 0 {
		j := (p.rng.Float64()*2 - 1) * float64(p.jitter)
		rtt += time.Duration(j)
		if rtt < time.Millisecond {
			rtt = time.Millisecond
		}
	}
	if p.rng.Float64() < p.spoofProb {
		return uint64(got), time.Duration(1+p.rng.Intn(3)) * time.Millisecond, true
	}
	return uint64(got), rtt, true
}

// TestRTTNoiseFloorRejected is the live-failure regression: 2ms spoof RTTs
// on an 80ms path must never enter the min filter, collapse BDP, or pin
// pacing to the floor.
func TestRTTNoiseFloorRejected(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	// Converge first on clean samples.
	p := &plant{linkBps: 10e6 / 8, rtt: 80 * time.Millisecond}
	runTicks(fc, ctl, p, 150, 100*time.Millisecond)
	// Now flood with scheduling artifacts alongside real traffic.
	for i := 0; i < 300; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{
			DeliveredBytes: 100000,
			Interval:       100 * time.Millisecond,
			RTT:            time.Duration(1 + i%3),
			HasRTT:         true,
		})
		if rtt := ctl.MinRTT(); rtt < minRTTFloor {
			t.Fatalf("tick %d: minRTT %v admitted below floor %v", i, rtt, minRTTFloor)
		}
		if pr := ctl.PacingRate(); pr < minPacingRate {
			t.Fatalf("tick %d: pacing %.0f below floor %.0f", i, pr, minPacingRate)
		}
		if w := ctl.WindowSegments(); w < 16 {
			t.Fatalf("tick %d: window %d below rails", i, w)
		}
	}
	// Real samples must still be accepted and lead back near truth.
	for i := 0; i < 50; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 100000, Interval: 100 * time.Millisecond, RTT: 80 * time.Millisecond, HasRTT: true})
	}
	if rtt := ctl.MinRTT(); rtt < minRTTFloor || rtt > 200*time.Millisecond {
		t.Fatalf("minRTT %v after recovery, want ~80ms", rtt)
	}
}

// TestStallHoldsThenTracks validates the horizon contract: a brief stall
// (shorter than the window) holds the estimate; a sustained degradation
// ages out and tracks down; restoration probes back up. No instant latch
// either direction.
func TestStallHoldsThenTracks(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	runTicks(fc, ctl, &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}, 150, 100*time.Millisecond)
	base := ctl.MaxBW()
	// 2s stall: backlogged but nothing delivered (inflight outstanding).
	for i := 0; i < 20; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 0, Interval: 100 * time.Millisecond, AppLimited: false})
	}
	if after := ctl.MaxBW(); math.Abs(after-base)/base > 0.1 {
		t.Fatalf("2s stall moved estimate %.0f -> %.0f, want hold", base, after)
	}
	if pr := ctl.PacingRate(); pr < minPacingRate {
		t.Fatalf("pacing %.0f below floor during stall", pr)
	}
	// 30s at 1/10th rate: must track down (old high points expire).
	runTicks(fc, ctl, &plant{linkBps: 1e6 / 8, rtt: 50 * time.Millisecond}, 300, 100*time.Millisecond)
	if bw := ctl.MaxBW(); bw > 1e6/8*2.5 {
		t.Fatalf("degraded estimate %.0f stuck far above 1Mbps link", bw)
	}
	// Restoration probes back up within 90s.
	runTicks(fc, ctl, &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}, 900, 100*time.Millisecond)
	if bw := ctl.MaxBW(); math.Abs(bw-10e6/8)/(10e6/8) > 0.5 {
		t.Fatalf("restored estimate %.0f, want ~1.25MB/s", bw)
	}
}

// TestRTTExpiryDamped is the 10.28s-minRTT regression: on an 84ms path,
// one queue-inflated sample after a stale filter must not teleport the
// estimate to seconds. Expiry rises at most 2x per episode toward the
// sample, and samples past the ceiling never enter at all.
func TestRTTExpiryDamped(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	p := &plant{linkBps: 10e6 / 8, rtt: 84 * time.Millisecond}
	runTicks(fc, ctl, p, 150, 100*time.Millisecond)
	// Go stale with no RTT input for 11s (backlogged, responses carry no
	// usable timing under flood).
	for i := 0; i < 110; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 100000, Interval: 100 * time.Millisecond})
	}
	// One 10s queue-position sample: past the 2s ceiling, must be dropped.
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{DeliveredBytes: 100000, Interval: 100 * time.Millisecond, RTT: 10 * time.Second, HasRTT: true})
	if rtt := ctl.MinRTT(); rtt > 200*time.Millisecond {
		t.Fatalf("ceiling breach admitted: minRTT %v, want ~84ms", rtt)
	}
	// A 1.5s inflated sample inside the ceiling: damped to <=2x, not a jump.
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{DeliveredBytes: 100000, Interval: 100 * time.Millisecond, RTT: 1500 * time.Millisecond, HasRTT: true})
	if rtt := ctl.MinRTT(); rtt > 200*time.Millisecond {
		t.Fatalf("expiry jump: minRTT %v, want damped near 84-168ms", rtt)
	}
	// Window must never pin at max from RTT inflation alone.
	if w := ctl.WindowSegments(); w > 512 {
		t.Fatalf("window %d near ceiling from inflated RTT", w)
	}
}

// TestSustainedIgnoresSpikes is the autorate-targeting regression: one
// huge tick among lows must move the max but barely move the sustained
// figure. Targeting burst peaks overdrives the link; targeting the mean
// does not.
func TestSustainedIgnoresSpikes(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	for i := 0; i < 50; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{DeliveredBytes: 50000, Interval: 100 * time.Millisecond})
	}
	sus := ctl.SustainedBW()
	if math.Abs(sus-500000)/500000 > 0.1 {
		t.Fatalf("sustained %.0f, want ~500000", sus)
	}
	// One 10x burst tick: max must take it (probe headroom lives there),
	// sustained must not notice at all (median immunity).
	fc.step(100 * time.Millisecond)
	ctl.Update(Sample{DeliveredBytes: 5000000, Interval: 100 * time.Millisecond})
	if bw := ctl.MaxBW(); bw < 40000000 {
		t.Fatalf("max %.0f did not take the burst", bw)
	}
	if sus2 := ctl.SustainedBW(); math.Abs(sus2-sus)/sus > 0.05 {
		t.Fatalf("sustained %.0f moved on one burst (base %.0f)", sus2, sus)
	}
}

// TestSustainedTracksDownAndHolds: sustained follows a real step-down
// within the horizon and holds through idle (app-limited ticks never
// enter the points).
func TestSustainedTracksDownAndHolds(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	runTicks(fc, ctl, &plant{linkBps: 10e6 / 8, rtt: 50 * time.Millisecond}, 100, 100*time.Millisecond)
	hi := ctl.SustainedBW()
	runTicks(fc, ctl, &plant{linkBps: 2e6 / 8, rtt: 50 * time.Millisecond}, 150, 100*time.Millisecond)
	lo := ctl.SustainedBW()
	if lo > hi*0.5 {
		t.Fatalf("sustained %.0f did not track 5x step-down from %.0f", lo, hi)
	}
	for i := 0; i < 30; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(Sample{Interval: 100 * time.Millisecond, AppLimited: true})
	}
	if after := ctl.SustainedBW(); math.Abs(after-lo)/lo > 0.05 {
		t.Fatalf("idle moved sustained %.0f -> %.0f", lo, after)
	}
}

// TestSendWindowSizing: one BDP in bytes each way, in each side's segment
// size, clamped to the same rails. 50KB BDP at 1200/500B segs wants
// ~42/~100 segs; tiny BDP floors both; huge BDP ceilings both.
func TestSendWindowSizing(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	cfg := testConfig(fc)
	cfg.MinSegs = 128
	cfg.MaxSegs = 1024
	cfg.SegBytes = 1200
	cfg.UpSegBytes = 500
	ctl := NewController(cfg)
	// Drive BDP to ~50KB: 1MB/s at 50ms.
	runTicks(fc, ctl, &plant{linkBps: 8e6 / 8, rtt: 50 * time.Millisecond}, 300, 100*time.Millisecond)
	if w := ctl.WindowSegments(); w != 128 {
		t.Fatalf("recv window %d, want floor 128 (BDP wants ~42)", w)
	}
	if s := ctl.SendWindowSegments(); s < 90 || s > 130 {
		t.Fatalf("send window %d, want ~100 segs for same bytes", s)
	}
	// Gigabit-ish BDP must ceiling both, never exceed MaxSegs.
	fc2 := &fclock{t: time.Unix(0, 0)}
	ctl2 := NewController(cfg)
	runTicks(fc2, ctl2, &plant{linkBps: 1e9 / 8, rtt: 10 * time.Millisecond}, 400, 100*time.Millisecond)
	if w := ctl2.WindowSegments(); w != 1024 {
		t.Fatalf("recv window %d, want ceiling 1024", w)
	}
	if s := ctl2.SendWindowSegments(); s != 1024 {
		t.Fatalf("send window %d, want ceiling 1024", s)
	}
}

// TestPureGigabitLink: clean 1Gbps/10ms path converges near line rate with
// the window pinned at ceiling and pacing alive. No collapse, no latch.
func TestPureGigabitLink(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	runTicks(fc, ctl, &plant{linkBps: 1e9 / 8, rtt: 10 * time.Millisecond}, 600, 100*time.Millisecond)
	if bw := ctl.MaxBW(); math.Abs(bw-1e9/8)/(1e9/8) > 0.3 {
		t.Fatalf("bw %.0f, want ~125MB/s", bw)
	}
	if rtt := ctl.MinRTT(); rtt != 10*time.Millisecond {
		t.Fatalf("rtt %v, want 10ms", rtt)
	}
	if pr := ctl.PacingRate(); pr < 1e9/8*0.5 {
		t.Fatalf("pacing %.0f starves a gigabit link", pr)
	}
}

// TestNoisyLinkSurvives: 15% loss with 25ms jitter on a 10Mbps/80ms path.
// The estimator must neither latch to zero nor pin to max: pacing stays
// within living bounds, window stays on rails, delivery progresses.
func TestNoisyLinkSurvives(t *testing.T) {
	fc := &fclock{t: time.Unix(0, 0)}
	ctl := NewController(testConfig(fc))
	rng := rand.New(rand.NewSource(7))
	var delivered uint64
	for i := 0; i < 600; i++ {
		dt := 100 * time.Millisecond
		want := ctl.PacingRate() * dt.Seconds()
		got := math.Min(want, 10e6/8*dt.Seconds())
		got *= 1 - 0.15*rng.Float64()*2 // bursty 0-30% loss around 15%
		jitter := time.Duration((rng.Float64()*2 - 1) * float64(25*time.Millisecond))
		rtt := 80*time.Millisecond + jitter
		if rtt < 10*time.Millisecond {
			rtt = 10 * time.Millisecond
		}
		fc.step(dt)
		ctl.Update(Sample{DeliveredBytes: uint64(got), Interval: dt, RTT: rtt, HasRTT: true})
		delivered += uint64(got)
		if pr := ctl.PacingRate(); pr < minPacingRate || pr > 100e6/8 {
			t.Fatalf("tick %d: pacing %.0f out of living bounds", i, pr)
		}
		if w := ctl.WindowSegments(); w < 16 || w > 2048 {
			t.Fatalf("tick %d: window %d off rails", i, w)
		}
	}
	if delivered == 0 {
		t.Fatal("zero total delivery on noisy link")
	}
	if bw := ctl.MaxBW(); bw <= 0 || bw > 10e6/8*1.5 {
		t.Fatalf("estimate %.0f outside plausible band", bw)
	}
}

// chaosScenario is one randomized real-world run. Seeds make failures repro.
type chaosScenario struct {
	seed      int64
	linkBps   float64
	baseRTT   time.Duration
	jitter    time.Duration
	lossFrac  float64
	policer   bool
	bwStepAt  int // tick index of a mid-run bandwidth step, -1 = none
	bwStepMul float64
	idleAt    int // tick index of a 3s app-limited gap, -1 = none
}

func runChaosScenario(t *testing.T, sc chaosScenario) {
	t.Helper()
	rng := rand.New(rand.NewSource(sc.seed))
	fc := &fclock{t: time.Unix(0, 0)}
	cfg := testConfig(fc)
	ctl := NewController(cfg)
	p := &chaosPlant{
		rng:       rng,
		linkBps:   sc.linkBps,
		baseRTT:   sc.baseRTT,
		jitter:    sc.jitter,
		lossFrac:  sc.lossFrac,
		spoofProb: 0.05, // background scheduling noise always present
	}
	if sc.policer {
		p.policerBps = sc.linkBps * (0.4 + 0.3*rng.Float64())
		p.burstBytes = float64(16000 + rng.Intn(48000))
		p.bucket = p.burstBytes
	}
	const ticks = 900 // 90 virtual seconds
	const dt = 100 * time.Millisecond
	var totalDelivered uint64
	stallTicks := 0
	for i := 0; i < ticks; i++ {
		if i == sc.bwStepAt {
			p.linkBps *= sc.bwStepMul
		}
		idle := sc.idleAt >= 0 && i >= sc.idleAt && i < sc.idleAt+30
		if idle {
			fc.step(dt)
			ctl.Update(Sample{DeliveredBytes: 8, Interval: dt, AppLimited: true})
			continue
		}
		got, rtt, hasRTT := p.tick(ctl.PacingRate(), dt)
		// Upstream wire to sustain this delivery: ~1 query per 16KB
		// response at ~500B each, plus one poll minimum.
		sent := got/32 + 500
		fc.step(dt)
		ctl.Update(Sample{
			DeliveredBytes: got,
			SentBytes:      sent,
			Interval:       dt,
			AppLimited:     false,
			RTT:            rtt,
			HasRTT:         hasRTT,
		})
		totalDelivered += got
		// Per-tick invariants: rails always hold, no zero latch, no blowup.
		if pr := ctl.PacingRate(); pr < minPacingRate || pr > 100e6/8 {
			t.Fatalf("seed %d tick %d: pacing %.0f out of rails", sc.seed, i, pr)
		}
		if w := ctl.WindowSegments(); w < cfg.MinSegs || w > cfg.MaxSegs {
			t.Fatalf("seed %d tick %d: window %d out of rails [%d,%d]", sc.seed, i, w, cfg.MinSegs, cfg.MaxSegs)
		}
		if s := ctl.SendWindowSegments(); s < cfg.MinSegs || s > cfg.MaxSegs {
			t.Fatalf("seed %d tick %d: send window %d out of rails [%d,%d]", sc.seed, i, s, cfg.MinSegs, cfg.MaxSegs)
		}
		if bw := ctl.MaxBW(); bw <= 0 || bw > 100e6/8 {
			t.Fatalf("seed %d tick %d: estimate %.0f out of rails", sc.seed, i, bw)
		}
		if rtt := ctl.MinRTT(); rtt < minRTTFloor || rtt > minRTTCeil {
			t.Fatalf("seed %d tick %d: minRTT %v out of rails", sc.seed, i, rtt)
		}
		if got == 0 {
			stallTicks++
		} else {
			stallTicks = 0
		}
		// Liveness: never stall backlogged delivery for 5s straight while
		// the link is up (the production deadlock: 0 B/s with DNS intact).
		if stallTicks > 50 {
			t.Fatalf("seed %d tick %d: 5s zero delivery on live link (deadlock)", sc.seed, i)
		}
	}
	if totalDelivered == 0 {
		t.Fatalf("seed %d: zero total delivery over 90s", sc.seed)
	}
	// Convergence on the stable tail: last 20s had no steps/idle, so the
	// estimate must be near the effective cap (policer or link, minus loss).
	stableTail := sc.bwStepAt < 0 || sc.bwStepAt < ticks-250
	if stableTail && (sc.idleAt < 0 || sc.idleAt < ticks-250) {
		want := p.capBps() * (1 - sc.lossFrac)
		if bw := ctl.MaxBW(); math.Abs(bw-want)/want > 0.6 {
			t.Fatalf("seed %d: estimate %.0f, want near cap %.0f (link %.0f policer %.0f loss %.2f)",
				sc.seed, bw, want, sc.linkBps, p.policerBps, sc.lossFrac)
		}
	}
}

// TestRandomizedRealWorld runs 60 seeded chaos scenarios spanning dial-up
// to 20Mbps, 20-200ms RTT with jitter, loss, policers, steps, and idle.
func TestRandomizedRealWorld(t *testing.T) {
	for s := int64(1); s <= 60; s++ {
		rng := rand.New(rand.NewSource(1000 + s))
		sc := chaosScenario{
			seed:     1000 + s,
			linkBps:  (0.5 + rng.Float64()*19.5) * 1e6 / 8,
			baseRTT:  time.Duration(20+rng.Intn(181)) * time.Millisecond,
			jitter:   time.Duration(rng.Intn(26)) * time.Millisecond,
			lossFrac: rng.Float64() * 0.03,
			policer:  rng.Float64() < 0.3,
			bwStepAt: -1,
			idleAt:   -1,
		}
		if rng.Float64() < 0.4 {
			sc.bwStepAt = 300 + rng.Intn(300)
			sc.bwStepMul = 0.3 + rng.Float64()*1.7 // 0.3x..2x step
		}
		if rng.Float64() < 0.3 {
			sc.idleAt = 200 + rng.Intn(500)
		}
		name := ""
		if sc.policer {
			name = "policed"
		} else {
			name = "open"
		}
		t.Run(name, func(t *testing.T) { runChaosScenario(t, sc) })
	}
}

// TestBurstLossAndJitterSpike hits the controller with correlated worst
// cases at once: 5% burst loss, 25ms jitter, policer, and a halving step
// mid-run. It must ride through without latching or stalling.
func TestBurstLossAndJitterSpike(t *testing.T) {
	sc := chaosScenario{
		seed:      424242,
		linkBps:   10e6 / 8,
		baseRTT:   80 * time.Millisecond,
		jitter:    25 * time.Millisecond,
		lossFrac:  0.05,
		policer:   true,
		bwStepAt:  450,
		bwStepMul: 0.5,
		idleAt:    200,
	}
	runChaosScenario(t, sc)
}
