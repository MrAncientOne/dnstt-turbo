package cake

import "time"

// Shaper ports the deficit-mode shaper from sch_cake.c
// (cake_advance_shaper + the cake_dequeue gate). Single-tin only, so the
// tin rate equals the global rate, but both clocks are kept exactly as in
// C: TinNext (b->time_next_packet), TimeNext (q->time_next_packet) and
// FailsafeNext (q->failsafe_next_packet). No burst parameter by design —
// deficit mode.
//
// All arithmetic mirrors the C fixed point: dur = len*rateNs>>rateShft.
type Shaper struct {
	RateNs   uint64
	RateShft uint8
	// TinNext is the tin clock (b->time_next_packet).
	TinNext time.Time
	// TimeNext is the global clock (q->time_next_packet).
	TimeNext time.Time
	// FailsafeNext unwedges a stalled shaper at 1.5x the rate. Dequeue
	// is gated only while BOTH TimeNext and FailsafeNext lie in the
	// future.
	FailsafeNext time.Time
}

// Enabled reports whether shaping is active (rateNs==0 means unlimited,
// sch_cake.c:cake_config_besteffort with rate 0).
func (s *Shaper) Enabled() bool { return s.RateNs != 0 }

// duration returns the shaper charge for length bytes.
func (s *Shaper) duration(length uint32) time.Duration {
	return time.Duration((uint64(length) * s.RateNs) >> s.RateShft)
}

// IdleReset ports the enqueue-side stale-state reset in
// sch_cake.c:cake_enqueue: when the queue was empty, clocks in the past
// must not delay the newly arrived chunk.
func (s *Shaper) IdleReset(now time.Time) {
	if !s.Enabled() {
		return
	}
	if s.TinNext.Before(now) {
		s.TinNext = now
	}
	if s.TimeNext.Before(now) {
		s.TimeNext = now
		s.FailsafeNext = now
	}
}

// Wait ports the cake_dequeue gate: while both TimeNext and FailsafeNext
// lie in the future, delivery must wait until the earlier of the two.
// Returns 0 when delivery may proceed immediately.
func (s *Shaper) Wait(now time.Time) time.Duration {
	if !s.Enabled() {
		return 0
	}
	if now.Before(s.TimeNext) && now.Before(s.FailsafeNext) {
		next := s.TimeNext
		if s.FailsafeNext.Before(next) {
			next = s.FailsafeNext
		}
		return next.Sub(now)
	}
	return 0
}

// Advance ports sch_cake.c:cake_advance_shaper for a delivered chunk
// (drop=false). The tin clock catches up gradually when behind and clamps
// when slightly ahead; the global clock advances unconditionally; the
// failsafe advances at 1.5x so a stalled shaper always unwinds. On a drop
// (drop=true) the failsafe is left alone, exactly as in C.
func (s *Shaper) Advance(length uint32, now time.Time, drop bool) {
	if !s.Enabled() {
		return
	}
	dur := s.duration(length)
	if s.TinNext.Before(now) {
		s.TinNext = s.TinNext.Add(dur)
	} else if s.TinNext.Before(now.Add(dur)) {
		s.TinNext = now.Add(dur)
	}
	s.TimeNext = s.TimeNext.Add(dur)
	if !drop {
		s.FailsafeNext = s.FailsafeNext.Add(dur + dur>>1)
	}
}
