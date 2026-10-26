package bbr

import (
	"testing"
	"time"
)

func TestRejectedPacketDoesNotLeaveQueuedDemand(t *testing.T) {
	f := NewFeedback(nil)
	p := make([]byte, 24+50)
	p[4], p[20] = 81, 50
	f.Queued(p)
	f.Unqueued(p)
	if f.queuedBytes != 0 {
		t.Fatalf("rejected packet left %d queued bytes", f.queuedBytes)
	}
}

func TestAuditFirstMeasuredRTTReplacesSeed(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewController(Config{Now: func() time.Time { return now }, SeedRTT: 100 * time.Millisecond})
	now = now.Add(200 * time.Millisecond)
	c.Update(Sample{Interval: 200 * time.Millisecond, DeliveredBytes: 50000, RTT: 200 * time.Millisecond, HasRTT: true})
	if c.MinRTT() != 200*time.Millisecond {
		t.Fatalf("first measured RTT is 200ms but window model uses %s", c.MinRTT())
	}
}

func TestLimitedSampleCanRevealHigherCapacity(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewController(Config{Now: func() time.Time { return now }})
	initial := c.MaxBW()
	now = now.Add(time.Second)
	c.Update(Sample{Interval: time.Second, DeliveredBytes: uint64(initial * 2), AppLimited: true})
	if c.MaxBW() < initial*2 {
		t.Fatalf("limited sample hid newly observed capacity: %f", c.MaxBW())
	}
	now = now.Add(time.Second)
	c.Update(Sample{Interval: time.Second, DeliveredBytes: 50, AppLimited: true})
	if c.MaxBW() < initial*2 {
		t.Fatal("tiny limited sample collapsed capacity")
	}
}

func TestShortFeedbackGapDoesNotEraseSeed(t *testing.T) {
	now := time.Unix(0, 0)
	c := NewController(Config{Now: func() time.Time { return now }, MinPacingBps: 1024})
	seed := c.MaxBW()
	now = now.Add(time.Second)
	c.Update(Sample{Interval: time.Second, SentBytes: 102, AppLimited: false})
	if c.MaxBW() != seed {
		t.Fatalf("one delayed ACK erased startup capacity: %.0f -> %.0f", seed, c.MaxBW())
	}
	// A sustained outage must still back off and retain a recovery probe.
	now = now.Add(9 * time.Second)
	c.Update(Sample{Interval: 9 * time.Second, AppLimited: false})
	if c.MaxBW() != 1024 {
		t.Fatalf("sustained outage did not back off: %.0f", c.MaxBW())
	}
}

func TestAuditHandshakeIsNotSaturationEvidence(t *testing.T) {
	now := time.Unix(0, 0)
	f := NewFeedback(func() time.Time { return now })
	// Real KCP framing: a 50-byte PUSH and an ACK with cumulative UNA=1.
	push := make([]byte, 24+50)
	push[0] = 1
	push[4] = 81
	push[20] = 50
	f.Sent(push)
	now = now.Add(200 * time.Millisecond)
	ack := make([]byte, 24)
	ack[0] = 1
	ack[4] = 82
	ack[16] = 1
	f.Received(ack)
	now = now.Add(200 * time.Millisecond)
	up, _ := f.Tick()
	if !up.AppLimited {
		t.Fatalf("completed 50-byte request is treated as saturated capacity: %+v", up)
	}
}
