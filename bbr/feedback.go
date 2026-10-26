package bbr

import (
	"encoding/binary"
	"sync"
	"time"
)

// KCPHeaderSize and MaxWindowSegments follow the pinned kcp-go wire format.
// Its advertised receive window is uint16; larger windows wrap on the wire.
const KCPHeaderSize = 24
const MaxWindowSegments = 65535

type kcpHeader struct {
	conv, sn, una uint32
	cmd           byte
	size          int
}

func walkKCP(p []byte, visit func(kcpHeader)) {
	for len(p) >= KCPHeaderSize {
		n := uint64(binary.LittleEndian.Uint32(p[20:24]))
		if n > uint64(len(p)-KCPHeaderSize) || p[4] < 81 || p[4] > 84 {
			return
		}
		visit(kcpHeader{binary.LittleEndian.Uint32(p), binary.LittleEndian.Uint32(p[12:]), binary.LittleEndian.Uint32(p[16:]), p[4], int(n)})
		p = p[KCPHeaderSize+int(n):]
	}
}

// PayloadBytes excludes ACKs, DNS framing and KCP headers. The upload pacer
// uses these same units as acknowledged delivery, avoiding an overhead-induced
// feedback loop. Retransmitted payload still consumes pacing budget.
func PayloadBytes(p []byte) int {
	n := 0
	walkKCP(p, func(h kcpHeader) {
		if h.cmd == 81 {
			n += h.size
		}
	})
	return n
}

type sentSegment struct {
	bytes         int
	at            time.Time
	retransmitted bool
}
type deliveryInterval struct {
	start           time.Time
	delivered, sent uint64
}

// Feedback observes actual KCP frames, independently of DNS response size.
// Upload delivery is credited only when ACK/UNA acknowledges a tracked PUSH.
// Download delivery counts each PUSH sequence once. State is per KCP session.
// Maps and ordering history are bounded by the protocol window, not uptime.
type Feedback struct {
	mu              sync.Mutex
	now             func() time.Time
	conv            uint32
	initialized     bool
	up              deliveryInterval
	down            deliveryInterval
	pending         map[uint32]sentSegment
	order           []uint32
	head            int
	highest         uint32
	haveSent        bool
	received        map[uint32]struct{}
	next            uint32
	rtt             time.Duration
	hasRTT          bool
	downSegment     int
	responseBytes   float64
	queuedBytes     uint64
	pendingBytes    uint64
	demandThreshold uint64
	fullDemand      bool
}

func NewFeedback(now func() time.Time) *Feedback {
	if now == nil {
		now = time.Now
	}
	return &Feedback{now: now, up: deliveryInterval{start: now()}, down: deliveryInterval{start: now()}, pending: make(map[uint32]sentSegment), received: make(map[uint32]struct{}), demandThreshold: 1024}
}

// Queued records payload offered by KCP before transport pacing. This observes
// demand independently of the rate that the pacer currently permits.
func (f *Feedback) Queued(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queuedBytes += uint64(PayloadBytes(p))
	if f.queuedBytes+f.pendingBytes >= f.demandThreshold {
		f.fullDemand = true
	}
}

// Unqueued removes demand for a packet rejected by the transport queue.
func (f *Feedback) Unqueued(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := uint64(PayloadBytes(p))
	if n >= f.queuedBytes {
		f.queuedBytes = 0
	} else {
		f.queuedBytes -= n
	}
}

// SetFlight sets the demand threshold from the current upload window. Two
// segments distinguish tiny handshakes from a flight; half a window observes
// saturation without requiring every ACK to arrive exactly at the tick.
func (f *Feedback) SetFlight(window, segment int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.demandThreshold = uint64(max(2*segment, window*segment/2))
}

// Sent must precede transport emission so a fast ACK cannot beat registration.
// A failed send remains outstanding until KCP retransmits or the session ends.
func (f *Feedback) Sent(p []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	n := uint64(PayloadBytes(p))
	if n > f.queuedBytes {
		f.queuedBytes = 0
	} else {
		f.queuedBytes -= n
	}
	walkKCP(p, func(h kcpHeader) {
		if !f.initialized {
			f.conv = h.conv
			f.initialized = true
		}
		if h.conv != f.conv || h.cmd != 81 {
			return
		}
		f.up.sent += uint64(h.size)
		if old, ok := f.pending[h.sn]; ok {
			old.retransmitted = true
			f.pending[h.sn] = old
			return
		}
		// Never re-credit a retransmission that was already acknowledged.
		if f.haveSent && int32(h.sn-f.highest) <= 0 {
			return
		}
		if len(f.pending) >= MaxWindowSegments {
			return
		}
		f.highest = h.sn
		f.haveSent = true
		f.pending[h.sn] = sentSegment{bytes: h.size, at: now}
		f.pendingBytes += uint64(h.size)
		f.order = append(f.order, h.sn)
	})
	if f.pendingBytes+f.queuedBytes >= f.demandThreshold {
		f.fullDemand = true
	}
}

func (f *Feedback) acknowledge(sn uint32, now time.Time) {
	seg, ok := f.pending[sn]
	if !ok {
		return
	}
	delete(f.pending, sn)
	f.pendingBytes -= uint64(seg.bytes)
	f.up.delivered += uint64(seg.bytes)
	// Karn's rule: an ACK after retransmission cannot identify the send.
	if rtt := now.Sub(seg.at); !seg.retransmitted && rtt > 0 && (!f.hasRTT || rtt < f.rtt) {
		f.rtt = rtt
		f.hasRTT = true
	}
}

func (f *Feedback) Received(p []byte) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	before := f.down.delivered
	walkKCP(p, func(h kcpHeader) {
		if !f.initialized || h.conv != f.conv {
			return
		}
		if f.haveSent && int32(h.una-(f.highest+1)) <= 0 {
			for f.head < len(f.order) && int32(f.order[f.head]-h.una) < 0 {
				f.acknowledge(f.order[f.head], now)
				f.head++
			}
		}
		if h.cmd == 82 {
			f.acknowledge(h.sn, now)
		}
		if h.cmd != 81 || int32(h.sn-f.next) < 0 || uint32(h.sn-f.next) >= MaxWindowSegments {
			return
		}
		if _, ok := f.received[h.sn]; ok {
			return
		}
		f.received[h.sn] = struct{}{}
		f.down.delivered += uint64(h.size)
		if h.size > f.downSegment {
			f.downSegment = h.size
		}
		for {
			if _, ok := f.received[f.next]; !ok {
				break
			}
			delete(f.received, f.next)
			f.next++
		}
	})
	for f.head < len(f.order) {
		if _, ok := f.pending[f.order[f.head]]; ok {
			break
		}
		f.head++
	}
	if f.head > 4096 && f.head > len(f.order)/2 {
		f.order = append([]uint32(nil), f.order[f.head:]...)
		f.head = 0
	}
	return int(f.down.delivered - before)
}

func intervalSample(d *deliveryInterval, now time.Time, active bool) Sample {
	// Keep delayed ACKs/short gaps in the same rate interval instead of
	// manufacturing alternating zero and burst samples on a jittery link.
	if d.delivered == 0 && active && now.Sub(d.start) < time.Second {
		return Sample{}
	}
	s := Sample{DeliveredBytes: d.delivered, SentBytes: d.sent, Interval: now.Sub(d.start), AppLimited: !active && d.delivered == 0}
	d.start = now
	d.delivered = 0
	d.sent = 0
	return s
}

func (f *Feedback) Tick() (upload, download Sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	upload = intervalSample(&f.up, now, len(f.pending) > 0 || f.up.sent > 0)
	download = intervalSample(&f.down, now, false)
	if upload.Interval > 0 {
		// A successful low-demand exchange must not replace capacity with
		// its application rate. Active zero-delivery samples still signal
		// outages, so a blocked path can back off.
		if upload.DeliveredBytes > 0 {
			upload.AppLimited = !f.fullDemand
		}
		f.fullDemand = f.queuedBytes+f.pendingBytes >= f.demandThreshold
	}
	if download.DeliveredBytes > 0 && download.DeliveredBytes < 512 {
		download.AppLimited = true
	}
	if upload.Interval > 0 {
		upload.RTT = f.rtt
		upload.HasRTT = f.hasRTT
		f.hasRTT = false
	}
	return
}

func (f *Feedback) DownstreamSegmentBytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downSegment
}

// Ignore ACK-only/duplicate responses when estimating payload per poll.
func (f *Feedback) RecordResponse(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n > 0 {
		if f.responseBytes == 0 {
			f.responseBytes = float64(n)
		} else {
			f.responseBytes = 0.9*f.responseBytes + 0.1*float64(n)
		}
	}
}
func (f *Feedback) AvgResponse() int { f.mu.Lock(); defer f.mu.Unlock(); return int(f.responseBytes) }
