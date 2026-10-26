package main

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkPollInflight(b *testing.B) {
	for _, depth := range []int{24, 256, 4096} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			m, _ := testMonitor()
			for i := 0; i < depth; i++ {
				m.sentQuery(100, uint16(i), true)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if m.PollInflight() != depth {
					b.Fatal("poll count changed")
				}
			}
		})
	}
}

func TestPollExpiryTracksChangingRTTAndLateReplies(t *testing.T) {
	m, clock := testMonitor()
	at := m.sentQuery(100, 1, true)
	clock.step(100 * time.Millisecond)
	m.sentQuery(100, 2, true)
	if m.PollInflight() != 2 {
		t.Fatal("lost initial polls")
	}
	m.cancelQuery(1, at)
	clock.step(950 * time.Millisecond)
	if m.PollInflight() != 1 {
		t.Fatal("cancellation expired a newer poll")
	}
	clock.step(50 * time.Millisecond)
	if m.PollInflight() != 0 {
		t.Fatal("did not expire at exact deadline")
	}
	m.receivedByID(0, 2)
	if m.Inflight() != 0 {
		t.Fatal("late response did not release ID")
	}
	m.sentQuery(100, 3, true)
	m.lastRTT = time.Second
	clock.step(1500 * time.Millisecond)
	if m.PollInflight() != 1 {
		t.Fatal("ignored increased RTT")
	}
	m.lastRTT = 20 * time.Millisecond
	if m.PollInflight() != 0 {
		t.Fatal("cached deadline ignored decreased RTT")
	}
	m.sentQuery(100, 4, true)
	if m.PollInflight() != 1 {
		t.Fatal("failed to reset after expiry")
	}
}
