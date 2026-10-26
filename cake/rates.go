package cake

// RateParams is the output of cake_set_rate: fixed-point shaper state plus
// auto-tuned AQM parameters. Field-for-field port of
// sch_cake.c:cake_set_rate (tin_rate_ns/tin_rate_shft, flow_quantum,
// cparams.target/interval/mtu_time/p_inc/p_dec).
type RateParams struct {
	RateNs      uint64 // time-per-byte in fixed point
	RateShft    uint8
	FlowQuantum uint16
	TargetNs    int64
	IntervalNs  int64
	MTUTimeNs   int64
	PInc        uint32
	PDec        uint32
	RateBps     uint64
}

// Default CoDel tuning from sch_cake.c:cake_config_init: interval 100ms,
// target 5ms (5-10% of interval per the CoDel RFC).
const (
	DefaultIntervalNs int64 = 100_000_000
	DefaultTargetNs   int64 = 5_000_000
	// DefaultRTTEstNs is the RTT estimate used to derive interval until
	// BBR feeds a measured MinRTT via SetRate.
	DefaultRTTEstNs int64  = 100_000_000
	minRateBps      uint64 = 64
)

// ComputeRateParams ports sch_cake.c:cake_set_rate.
// rateBps==0 means unlimited (zero delay, quantum 1514, AQM untouched
// beyond mtu==0 early return semantics: caller keeps previous target).
// mtu is the max chunk/segment size (serialisation-delay basis).
// targetNs/rttEstNs seed the target/interval auto-tuning.
func ComputeRateParams(rateBps uint64, mtu uint32, targetNs, rttEstNs int64) RateParams {
	rp := RateParams{RateBps: rateBps}
	if rateBps != 0 {
		q := rateBps >> 12
		switch {
		case q < 300:
			q = 300
		case q > 1514:
			q = 1514
		}
		rp.FlowQuantum = uint16(q)
		shft := uint8(34)
		rateNs := (uint64(1_000_000_000) << shft) / maxU64(minRateBps, rateBps)
		for rateNs>>34 != 0 {
			rateNs >>= 1
			shft--
		}
		rp.RateNs = rateNs
		rp.RateShft = shft
	} else {
		rp.FlowQuantum = 1514
	}
	if mtu == 0 {
		return rp
	}
	byteTargetNs := int64((uint64(mtu) * rp.RateNs) >> rp.RateShft)
	target := byteTargetNs * 3 / 2
	if target < targetNs {
		target = targetNs
	}
	interval := rttEstNs + target - targetNs
	if interval < target*2 {
		interval = target * 2
	}
	rp.TargetNs = target
	rp.IntervalNs = interval
	rp.MTUTimeNs = byteTargetNs
	rp.PInc = 1 << 24 // 1/256
	rp.PDec = 1 << 20 // 1/4096
	return rp
}

// BufferLimit ports sch_cake.c:cake_reconfigure's default buffer sizing:
// max(rate*interval/4, 4MiB); 0 rate (unlimited) means no kernel cap, which
// the tunnel maps to a fixed 4MiB cap so a flood still has a bound.
func BufferLimit(rateBps uint64, intervalNs int64) uint64 {
	const minBuf = uint64(4 << 20)
	if rateBps == 0 {
		return minBuf
	}
	// rate*interval/4 in bytes: rate(B/s)*interval(ns)/1e9/4.
	buf := rateBps * uint64(intervalNs) / 1_000_000_000 / 4
	if buf < minBuf {
		buf = minBuf
	}
	return buf
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
