package cake

import (
	"time"
)

// CobaltParams mirrors sch_cake.c:struct cobalt_params.
// All durations are in nanoseconds as int64 to keep the fixed-point
// shaper/control-law arithmetic exact.
type CobaltParams struct {
	Interval int64  // codel initial drop rate (ns)
	Target   int64  // max persistent sojourn time (ns)
	MTUTime  int64  // serialisation delay of max-size chunk (ns)
	PInc     uint32 // BLUE increment (Q0.32)
	PDec     uint32 // BLUE decrement (Q0.32)
}

// CobaltVars mirrors sch_cake.c:struct cobalt_vars.
type CobaltVars struct {
	Count      uint32
	RecInvSqrt uint32
	DropNext   time.Time
	BlueTimer  time.Time
	PDrop      uint32
	Dropping   bool
	ECNMarked  bool
}

// recInvSqrtCache mirrors sch_cake.c:inv_sqrt_cache (REC_INV_SQRT_CACHE=16).
var recInvSqrtCache = [16]uint32{
	0xFFFFFFFF, 0xFFFFFFFF, 3037000500, 2479700525,
	2147483647, 1920767767, 1753413056, 1623345051,
	1518500250, 1431655765, 1358187914, 1294981364,
	1239850263, 1191209601, 1147878294, 1108955788,
}

// cobaltNewtonStep mirrors sch_cake.c:cobalt_newton_step.
func cobaltNewtonStep(v *CobaltVars, count uint32) {
	invsqrt := uint64(v.RecInvSqrt)
	invsqrt2 := (invsqrt * invsqrt) >> 32
	val := (3 << 32) - uint64(count)*invsqrt2
	val >>= 2
	val = (val * invsqrt) >> (32 - 2 + 1)
	v.RecInvSqrt = uint32(val)
}

// cobaltInvsqrt mirrors sch_cake.c:cobalt_invsqrt.
func cobaltInvsqrt(v *CobaltVars, count uint32) {
	if count < uint32(len(recInvSqrtCache)) {
		v.RecInvSqrt = recInvSqrtCache[count]
		return
	}
	cobaltNewtonStep(v, count)
}

// cobaltControl mirrors sch_cake.c:cobalt_control:
// t + interval/sqrt(count), via reciprocal_scale(interval, rec).
func cobaltControl(t time.Time, intervalNs int64, recInvSqrt uint32) time.Time {
	// reciprocal_scale: (interval * rec) >> 32.
	delta := int64((uint64(intervalNs) * uint64(recInvSqrt)) >> 32)
	return t.Add(time.Duration(delta))
}

// CobaltQueueFull mirrors sch_cake.c:cobalt_queue_full (overlimit path).
func CobaltQueueFull(v *CobaltVars, p *CobaltParams, now time.Time) {
	if now.Sub(v.BlueTimer) > time.Duration(p.Target) {
		pDrop := v.PDrop
		v.PDrop += p.PInc
		if v.PDrop < p.PInc {
			v.PDrop = ^uint32(0)
		}
		_ = pDrop
		v.BlueTimer = now
	}
	v.Dropping = true
	v.DropNext = now
	if v.Count == 0 {
		v.Count = 1
	}
}

// CobaltQueueEmpty mirrors sch_cake.c:cobalt_queue_empty.
func CobaltQueueEmpty(v *CobaltVars, p *CobaltParams, now time.Time) {
	if v.PDrop != 0 && now.Sub(v.BlueTimer) > time.Duration(p.Target) {
		if v.PDrop < p.PDec {
			v.PDrop = 0
		} else {
			v.PDrop -= p.PDec
		}
		v.BlueTimer = now
	}
	v.Dropping = false
	if v.Count != 0 && !now.Before(v.DropNext) {
		v.Count--
		cobaltInvsqrt(v, v.Count)
		v.DropNext = cobaltControl(v.DropNext, p.Interval, v.RecInvSqrt)
	}
}

// CobaltDecision is the outcome of CobaltShouldDrop.
type CobaltDecision int

const (
	// CobaltDeliver mirrors QDISC_DROP_UNSPEC (deliver the chunk).
	CobaltDeliver CobaltDecision = iota
	// CobaltDrop mirrors a drop reason (congested or BLUE flood).
	CobaltDrop
)

// CobaltShouldDrop mirrors sch_cake.c:cobalt_should_drop, minus the skb ECN
// mark attempt (no ECN wire channel here; ECN-capable would still be
// counted, but the action is drop either way).
//
// enqueueTime is the chunk's enqueue timestamp (cobalt_get_enqueue_time),
// bulkFlows is the tin's bulk flow count (BLUE-gated CoDel target scaling).
// randU32 supplies the BLUE coin flip; pass a deterministic value in tests.
func CobaltShouldDrop(v *CobaltVars, p *CobaltParams, now, enqueueTime time.Time, bulkFlows int, randU32 uint32) (CobaltDecision, bool) {
	sojourn := now.Sub(enqueueTime)
	overTarget := sojourn > time.Duration(p.Target) &&
		sojourn > time.Duration(p.MTUTime)*time.Duration(bulkFlows)*2 &&
		sojourn > time.Duration(p.MTUTime)*4
	count := v.Count
	nextDue := count != 0 && !now.Before(v.DropNext)

	v.ECNMarked = false

	if overTarget {
		if !v.Dropping {
			v.Dropping = true
			v.DropNext = cobaltControl(now, p.Interval, v.RecInvSqrt)
		}
		if count == 0 {
			count = 1
		}
	} else if v.Dropping {
		v.Dropping = false
	}

	decision := CobaltDeliver
	if nextDue && v.Dropping {
		// No ECN channel: every due signal is a drop.
		// (sch_cake.c tries INET_ECN_set_ce first.)
		decision = CobaltDrop
		count++
		if count == 0 {
			count--
		}
		cobaltInvsqrt(v, count)
		v.DropNext = cobaltControl(v.DropNext, p.Interval, v.RecInvSqrt)
	} else {
		for nextDue {
			count--
			cobaltInvsqrt(v, count)
			v.DropNext = cobaltControl(v.DropNext, p.Interval, v.RecInvSqrt)
			nextDue = count != 0 && !now.Before(v.DropNext)
		}
	}

	// Simple BLUE, sch_cake.c: deliberate lack of ECN here too.
	if v.PDrop != 0 && decision == CobaltDeliver && randU32 < v.PDrop {
		decision = CobaltDrop
	}

	v.Count = count
	if count == 0 {
		v.DropNext = now.Add(time.Duration(p.Interval))
	} else if decision == CobaltDeliver && now.After(v.DropNext) {
		v.DropNext = now
	}
	return decision, v.ECNMarked
}
