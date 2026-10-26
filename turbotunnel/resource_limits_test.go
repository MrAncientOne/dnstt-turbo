package turbotunnel

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestPollLookupDoesNotAllocateRemote(t *testing.T) {
	m := NewRemoteMap(0)
	for i := 0; i < 10000; i++ {
		var id ClientID
		binary.LittleEndian.PutUint64(id[:], uint64(i))
		out, stash := m.ExistingQueues(id)
		if out != nil || stash != nil {
			t.Fatal("unknown poll created queues")
		}
	}
	if len(m.inner.byAge) != 0 || m.inner.queueSlots != 0 {
		t.Fatal("polls retained remote state")
	}
	m.SendQueue(ClientID{}) <- []byte("data")
	out, _ := m.ExistingQueues(ClientID{})
	if string(<-out) != "data" {
		t.Fatal("existing peer lost data")
	}
}

func TestRemoteLimitsPreserveExistingPeersAndExpire(t *testing.T) {
	old := GetQueueSize()
	defer SetQueueSize(old)
	for _, size := range []int{1, 131070} {
		SetQueueSize(size)
		m := NewRemoteMap(0)
		accepted := 0
		for i := 0; i < maxRemoteRecords+1; i++ {
			var id ClientID
			binary.LittleEndian.PutUint64(id[:], uint64(i))
			if q := m.SendQueue(id); q != nil {
				accepted++
				q <- []byte{1}
			}
		}
		want := min(maxRemoteRecords, maxRemoteQueueSlots/size)
		if accepted != want {
			t.Fatalf("size=%d admitted=%d want=%d", size, accepted, want)
		}
		if m.inner.queueSlots > maxRemoteQueueSlots {
			t.Fatal("queue slot budget exceeded")
		}
		out, _ := m.ExistingQueues(ClientID{})
		if <-out == nil {
			t.Fatal("live peer evicted")
		}
		m.inner.removeExpired(time.Now().Add(time.Hour), time.Minute)
		if m.inner.queueSlots != 0 || len(m.inner.byAge) != 0 {
			t.Fatal("expiry did not release budget")
		}
		if m.SendQueue(ClientID{}) == nil {
			t.Fatal("expired budget not reusable")
		}
	}
}
