package main

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
	"www.bamsoftware.com/git/dnstt.git/bbr"
)

func kcpTestFrame(cmd byte, n int) []byte {
	p := make([]byte, 24+n)
	p[4] = cmd
	binary.LittleEndian.PutUint32(p[20:], uint32(n))
	return p
}
func TestAdaptiveIndependentDirections(t *testing.T) {
	now := time.Unix(1, 0)
	a := newAdaptiveControl(500, func() time.Time { return now })
	var d adaptiveDecision
	for i := 0; i < 40; i++ {
		now = now.Add(100 * time.Millisecond)
		d = a.update(bbr.Sample{DeliveredBytes: 125000, SentBytes: 125000, Interval: 100 * time.Millisecond, RTT: 80 * time.Millisecond, HasRTT: true}, bbr.Sample{DeliveredBytes: 6250000, Interval: 100 * time.Millisecond, RTT: 80 * time.Millisecond, HasRTT: true}, 1376, 16000)
	}
	if a.up.MaxBW() != 1250000 || a.down.MaxBW() != 62500000 {
		t.Fatalf("direction mix: up=%f down=%f", a.up.MaxBW(), a.down.MaxBW())
	}
	if d.receiveWindow <= 1024 || d.sendWindow >= d.receiveWindow {
		t.Fatalf("asymmetric windows %+v", d)
	}
	if d.shaperRate != d.pacingRate {
		t.Fatal("slow-path admission shaper must permit carrier probing")
	}
	if d.pacingRate > 4*1250000 {
		t.Fatal("download feeds upload pacer")
	}
	if d.receiveWindow*1376 > bbrMaxFlight || d.sendWindow*476 > bbrMaxFlight {
		t.Fatal("unbounded flight")
	}
}
func TestAdaptiveNoFixed500MbpsLimit(t *testing.T) {
	now := time.Unix(1, 0)
	a := newAdaptiveControl(1400, func() time.Time { return now })
	now = now.Add(100 * time.Millisecond)
	s := bbr.Sample{DeliveredBytes: 12500000, SentBytes: 12500000, Interval: 100 * time.Millisecond, RTT: 40 * time.Millisecond, HasRTT: true}
	d := a.update(s, s, 1376, 16000)
	if a.up.MaxBW() != 125000000 || d.sendWindow*1376 < 10000000 {
		t.Fatalf("1Gbps model capped: %+v", d)
	}
}

func TestAdaptiveAdmissionShapingFollowsUploadCapacity(t *testing.T) {
	now := time.Unix(1, 0)
	a := newAdaptiveControl(1400, func() time.Time { return now })
	step := func(rate float64) adaptiveDecision {
		now = now.Add(100 * time.Millisecond)
		s := bbr.Sample{DeliveredBytes: uint64(rate / 10), SentBytes: uint64(rate / 10), Interval: 100 * time.Millisecond, RTT: 20 * time.Millisecond, HasRTT: true}
		return a.update(s, s, 1376, 16000)
	}
	threshold := float64(fairChunk) * 1000
	var d adaptiveDecision
	for i := 0; i < 100; i++ {
		d = step(threshold * 2)
	}
	if d.shaperRate != 0 || d.pacingRate <= 0 {
		t.Fatal("high-capacity path still has two pacing clocks")
	}
	for i := 0; i < 100; i++ {
		d = step(threshold * 0.75)
	}
	if d.shaperRate != 0 {
		t.Fatal("hysteresis did not retain carrier-only pacing")
	}
	for i := 0; i < 100; i++ {
		d = step(threshold / 4)
	}
	if d.shaperRate != d.pacingRate || d.shaperRate <= 0 {
		t.Fatal("capacity drop did not restore slow-path admission")
	}
}

func TestAdaptiveFlightCoversKCPFlushCadence(t *testing.T) {
	now := time.Unix(1, 0)
	a := newAdaptiveControl(1400, func() time.Time { return now })
	now = now.Add(100 * time.Millisecond)
	s := bbr.Sample{DeliveredBytes: 1000000, SentBytes: 1000000, Interval: 100 * time.Millisecond, RTT: 20 * time.Millisecond, HasRTT: true}
	d := a.update(s, s, 1376, 16000)
	// KCP may only flush once per 100ms. A physical 20ms RTT must not
	// shrink the flight below what is needed between those flushes.
	const required = 2000000 // 10 MB/s * 100ms * window gain 2
	if d.sendWindow*1376 < required || d.receiveWindow*1376 < required {
		t.Fatalf("flush cadence throttles fast link: %+v", d)
	}
	if a.up.MinRTT() != 20*time.Millisecond || a.down.MinRTT() != 20*time.Millisecond {
		t.Fatal("flight headroom must not falsify measured RTT")
	}
	if d.polls < required/16000 {
		t.Fatalf("poll budget cannot service receive flight: %+v", d)
	}
}

// The real DNS send path must service both reverse ACKs and polling while
// upload tokens are exhausted; this test uses no OS network sockets.
func TestPacedUploadServicesControlAndPoll(t *testing.T) {
	c := testDNSConn(testPathDomain(t), true)
	defer c.QueuePacketConn.Close()
	c.pacer = bbr.NewPacer(nil)
	c.pacer.SetRate(1, 1500)
	c.pacer.Advance(1500)
	c.controlChan = make(chan []byte, 2)
	c.controlChan <- kcpTestFrame(82, 0)
	c.pollChan <- struct{}{}
	tr := &notifyingTransport{written: make(chan []byte, 4)}
	done := make(chan error, 1)
	go func() { done <- c.send(tr, kcpTestFrame(81, 500), captureAddr{}) }()
	for i := 0; i < 2; i++ {
		select {
		case <-tr.written:
		case <-time.After(time.Second):
			t.Fatal("control/poll blocked behind upload pacing")
		}
	}
	c.QueuePacketConn.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("paced upload ignored cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("send did not stop")
	}
}

type notifyingTransport struct {
	captureTransport
	written chan []byte
}

func (n *notifyingTransport) WriteTo(p []byte, _ net.Addr) (int, error) {
	n.written <- append([]byte(nil), p...)
	return len(p), nil
}
