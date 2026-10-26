package bbr

import (
	"bytes"
	"encoding/binary"
	kcp "github.com/xtaci/kcp-go/v5"
	"testing"
	"time"
)

func frame(cmd byte, sn, una uint32, n int) []byte {
	p := make([]byte, KCPHeaderSize+n)
	binary.LittleEndian.PutUint32(p, 7)
	p[4] = cmd
	binary.LittleEndian.PutUint32(p[12:], sn)
	binary.LittleEndian.PutUint32(p[16:], una)
	binary.LittleEndian.PutUint32(p[20:], uint32(n))
	return p
}
func TestFeedbackAcknowledgesUploadOnce(t *testing.T) {
	now := time.Unix(1, 0)
	f := NewFeedback(func() time.Time { return now })
	f.Sent(frame(81, 0, 0, 500))
	f.Sent(frame(81, 1, 0, 700))
	now = now.Add(80 * time.Millisecond)
	f.Received(frame(82, 1, 0, 0))
	f.Received(frame(82, 1, 0, 0))
	up, down := f.Tick()
	if up.DeliveredBytes != 700 || down.DeliveredBytes != 0 || up.RTT != 80*time.Millisecond {
		t.Fatalf("up=%+v down=%+v", up, down)
	}
	f.Sent(frame(81, 0, 0, 500))
	now = now.Add(100 * time.Millisecond)
	// Cumulative ACK recovers a lost explicit ACK. Retransmissions neither
	// inflate delivered bytes nor produce ambiguous RTT samples.
	f.Received(frame(84, 0, 2, 0))
	f.Received(frame(82, 0, 2, 0))
	up, _ = f.Tick()
	if up.DeliveredBytes != 500 || up.HasRTT || len(f.pending) != 0 {
		t.Fatalf("%+v pending=%d", up, len(f.pending))
	}
	f.Sent(frame(81, 0, 0, 500))
	f.Received(frame(82, 0, 2, 0))
	now = now.Add(time.Second)
	up, _ = f.Tick()
	if up.DeliveredBytes != 0 {
		t.Fatal("re-credited acknowledged retransmission")
	}
}
func TestFeedbackDownloadReorderingAndControl(t *testing.T) {
	now := time.Unix(1, 0)
	f := NewFeedback(func() time.Time { return now })
	f.Sent(frame(84, 0, 0, 0))
	f.Received(frame(81, 1, 0, 1400))
	f.Received(frame(81, 1, 0, 1400))
	f.Received(frame(81, 0, 0, 1000))
	f.Received(frame(81, 0, 0, 1000))
	for i := 0; i < 100; i++ {
		f.Received(frame(82, uint32(i), 0, 0))
		f.RecordResponse(0)
	}
	f.RecordResponse(2400)
	now = now.Add(100 * time.Millisecond)
	up, down := f.Tick()
	if !up.AppLimited || up.DeliveredBytes != 0 || down.DeliveredBytes != 2400 || len(f.received) != 0 || f.AvgResponse() != 2400 {
		t.Fatalf("%+v %+v", up, down)
	}
}
func TestFeedbackJitterAndInvalidFrames(t *testing.T) {
	now := time.Unix(1, 0)
	f := NewFeedback(func() time.Time { return now })
	f.Sent(frame(81, 0, 0, 500))
	for i := 0; i < 4; i++ {
		now = now.Add(100 * time.Millisecond)
		up, _ := f.Tick()
		if up.Interval != 0 {
			t.Fatalf("split jitter gap: %+v", up)
		}
	}
	bad := frame(82, 0, 1, 0)
	bad[0] = 8
	f.Received(bad)
	f.Received(frame(84, 0, 100, 0)) // impossible cumulative acknowledgement
	now = now.Add(100 * time.Millisecond)
	f.Received(frame(82, 0, 1, 0))
	up, _ := f.Tick()
	if up.DeliveredBytes != 500 || up.Interval != 500*time.Millisecond {
		t.Fatalf("%+v", up)
	}
	malformed := frame(81, 0, 0, 10)
	binary.LittleEndian.PutUint32(malformed[20:], 100)
	if PayloadBytes(malformed) != 0 || PayloadBytes(frame(81, 0, 0, 50)) != 50 || PayloadBytes(frame(82, 0, 0, 0)) != 0 {
		t.Fatal("payload accounting")
	}
}
func TestControllerOutageRecoveryAndFlightBudget(t *testing.T) {
	now := time.Unix(1, 0)
	c := NewController(Config{Now: func() time.Time { return now }, MaxSegs: MaxWindowSegments, MinSegs: 32, SegBytes: 1376, MaxFlightBytes: 16 << 20, MinPacingBps: 1024, WindowGain: 2})
	step := func(bytes uint64) {
		now = now.Add(100 * time.Millisecond)
		c.Update(Sample{DeliveredBytes: bytes, Interval: 100 * time.Millisecond, RTT: 80 * time.Millisecond, HasRTT: true})
	}
	for i := 0; i < 20; i++ {
		step(6250000)
	} // 500 Mbps
	if c.WindowSegments() <= 1024 {
		t.Fatal("old fixed ceiling remains")
	}
	if c.WindowSegments()*1376 > 16<<20 {
		t.Fatal("flight budget exceeded")
	}
	for i := 0; i < 100; i++ {
		step(0)
	}
	if c.PacingRate() > 4096 {
		t.Fatalf("no outage backoff: %f", c.PacingRate())
	}
	step(125000)
	if c.State() != StateStartup || c.MaxBW() != 1250000 {
		t.Fatalf("recovery %s rate=%f", c.State(), c.MaxBW())
	}
	for i := 0; i < 100; i++ {
		step(800)
	} // 64 kbps
	if c.MaxBW() > 8000.1 {
		t.Fatalf("does not follow permanent drop: %f", c.MaxBW())
	}
}

// Exercise the pinned implementation's actual segmentation, ACK batching and
// cumulative acknowledgements, without requiring an OS network socket.
func TestFeedbackPinnedKCP(t *testing.T) {
	now := time.Unix(1, 0)
	f := NewFeedback(func() time.Time { return now })
	var wire, acks [][]byte
	sender := kcp.NewKCP(7, func(p []byte, n int) {
		p = append([]byte(nil), p[:n]...)
		f.Sent(p)
		wire = append(wire, p)
	})
	receiver := kcp.NewKCP(7, func(p []byte, n int) { acks = append(acks, append([]byte(nil), p[:n]...)) })
	sender.NoDelay(0, 10, 2, 1)
	payload := bytes.Repeat([]byte("integrity"), 400)
	if n := sender.Send(payload); n != 0 {
		t.Fatalf("Send: %d", n)
	}
	sender.Update()
	if len(wire) == 0 {
		t.Fatal("no KCP output")
	}
	now = now.Add(80 * time.Millisecond)
	for _, p := range wire {
		if n := receiver.Input(p, true, true); n != 0 {
			t.Fatalf("Input: %d", n)
		}
	}
	for _, p := range acks {
		f.Received(p)
		f.Received(p)
	}
	up, _ := f.Tick()
	if up.DeliveredBytes != uint64(len(payload)) {
		t.Fatalf("credited %+v, want %d", up, len(payload))
	}
	got := make([]byte, len(payload))
	if n := receiver.Recv(got); n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("Recv: %d", n)
	}
}

func TestFeedbackSequenceWrap(t *testing.T) {
	now := time.Unix(1, 0)
	f := NewFeedback(func() time.Time { return now })
	f.next = ^uint32(0)
	f.Sent(frame(81, ^uint32(0), 0, 100))
	f.Sent(frame(81, 0, 0, 200))
	now = now.Add(100 * time.Millisecond)
	f.Received(frame(84, 0, 1, 0))
	f.Received(frame(81, 0, 1, 200))
	f.Received(frame(81, ^uint32(0), 1, 100))
	f.Received(frame(81, 0, 1, 200))
	up, down := f.Tick()
	if up.DeliveredBytes != 300 || down.DeliveredBytes != 300 || len(f.pending) != 0 || len(f.received) != 0 {
		t.Fatalf("up=%+v down=%+v", up, down)
	}
}
