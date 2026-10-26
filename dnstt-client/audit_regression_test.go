package main

import (
	"encoding/binary"
	"strconv"
	"testing"
	"time"
	"www.bamsoftware.com/git/dnstt.git/bbr"
)

// Legitimate server -kcp-mtu-cap200 gives176 payload bytes per KCP segment.
// Drive actual feedback -> adaptive.update to check advertised flight units.
func TestAuditSmallDownstreamSegmentsSizeActualFlight(t *testing.T) {
	for _, payload := range []int{176, 1376} {
		t.Run(strconv.Itoa(payload), func(t *testing.T) {
			now := time.Unix(1, 0)
			feedback := bbr.NewFeedback(func() time.Time { return now })
			a := newAdaptiveControl(500, func() time.Time { return now })
			feedback.Sent(kcpTestFrame(81, 32)) // actual conversation initialization
			var sn uint32
			var d adaptiveDecision
			for tick := 0; tick < 4; tick++ {
				now = now.Add(100 * time.Millisecond)
				for i := 0; i < 1000; i++ {
					packet := kcpTestFrame(81, payload)
					binary.LittleEndian.PutUint32(packet[12:], sn)
					sn++
					feedback.RecordResponse(feedback.Received(packet))
				}
				up, down := feedback.Tick()
				down.RTT = 100 * time.Millisecond
				down.HasRTT = true
				d = a.update(up, down, feedback.DownstreamSegmentBytes(), feedback.AvgResponse())
			}
			actualFlight := d.receiveWindow * payload
			required := 2 * 1000 * payload //2x delivered bytes per100ms service/RTT horizon
			t.Logf("observed payload=%d model segment=%d receive window=%d actual flight=%d desired=%d", payload, a.downSegment, d.receiveWindow, actualFlight, required)
			if actualFlight < required {
				t.Fatalf("adaptive advertised flight uses wrong segment units: got%d bytes want>=%d", actualFlight, required)
			}
		})
	}
}
