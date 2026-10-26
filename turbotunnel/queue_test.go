package turbotunnel

import (
	"sync"
	"testing"
	"time"
)

func TestTryWriteToReportsAcceptance(t *testing.T) {
	old := GetQueueSize()
	SetQueueSize(1)
	defer SetQueueSize(old)
	c := NewQueuePacketConn(DummyAddr{}, 0)
	defer c.Close()
	if _, accepted, err := c.TryWriteTo([]byte{1}, DummyAddr{}); !accepted || err != nil {
		t.Fatalf("empty queue: accepted=%v error=%v", accepted, err)
	}
	if n, accepted, err := c.TryWriteTo([]byte{2}, DummyAddr{}); n != 1 || accepted || err != nil {
		t.Fatalf("full queue: n=%d accepted=%v error=%v", n, accepted, err)
	}
	c.Close()
	if _, accepted, err := c.TryWriteTo([]byte{3}, DummyAddr{}); accepted || err == nil {
		t.Fatalf("closed queue: accepted=%v error=%v", accepted, err)
	}
}

// TestWriteToFullReportsSuccess: a full send queue drops the packet but
// still returns (len, nil) — the drop must look like network loss so KCP
// retransmits. Returning an error here would kill the session (kcp-go
// treats PacketConn write errors as fatal), turning momentary bursts into
// session death.
func TestWriteToFullReportsSuccess(t *testing.T) {
	old := GetQueueSize()
	SetQueueSize(4)
	defer SetQueueSize(old)
	c := NewQueuePacketConn(DummyAddr{}, 0)
	addr := DummyAddr{}
	// Fill the 4-slot queue through WriteTo itself.
	for i := 0; i < 4; i++ {
		if n, err := c.WriteTo([]byte{byte(i)}, addr); n != 1 || err != nil {
			t.Fatalf("fill WriteTo = (%d, %v), want (1, nil)", n, err)
		}
	}
	n, err := c.WriteTo([]byte{0xaa, 0xbb}, addr)
	if n != 2 || err != nil {
		t.Fatalf("full WriteTo = (%d, %v), want (2, nil): drop must mimic loss", n, err)
	}
}

// TestExpiredQueueDroppedFromMap: after expiry the record is forgotten and
// Lookup mints a fresh usable queue (no panic, no stale data). Expiry
// deliberately does not close the old queue: closing races with an
// in-flight Lookup+send on another goroutine (send on closed channel
// panics), and every in-repo receiver re-fetches or selects with a
// timeout, so abandonment is safe.
func TestExpiredQueueDroppedFromMap(t *testing.T) {
	old := GetQueueSize()
	SetQueueSize(8)
	defer SetQueueSize(old)
	m := NewRemoteMap(20 * time.Millisecond)
	addr := DummyAddr{}
	m.SendQueue(addr) <- []byte{0x01}
	time.Sleep(100 * time.Millisecond) // multiple expiry sweeps
	// The old queue was dropped; Lookup minted a fresh one. A receive must
	// not panic; on the fresh queue it blocks, so just verify send works.
	select {
	case m.SendQueue(addr) <- []byte{0x02}:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh post-expiry queue not writable")
	}
	select {
	case p := <-m.SendQueue(addr):
		if len(p) != 1 || p[0] != 0x02 {
			t.Fatalf("post-expiry queue held %x, want fresh data", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fresh post-expiry queue not readable")
	}
}

// TestExpiryVsSendRace is the regression for a whole-process crash: the
// expiry sweeper once closed send queues while another goroutine held the
// same queue from Lookup and was about to send (send on closed channel
// panics). Hammer Lookup+send against aggressive expiry under -race: any
// panic or race fails.
func TestExpiryVsSendRace(t *testing.T) {
	old := GetQueueSize()
	SetQueueSize(16)
	defer SetQueueSize(old)
	m := NewRemoteMap(2 * time.Millisecond)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				select {
				case m.SendQueue(DummyAddr{}) <- []byte{0x01}:
				default:
				}
				_ = m.Stash(DummyAddr{}, []byte{0x02})
				_ = m.Unstash(DummyAddr{})
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCloseThenOps: after Close, ReadFrom/WriteTo fail with the closed
// error (never panic on the un-set atomic), QueueIncoming drops silently,
// and double Close reports the stored error.
func TestCloseThenOps(t *testing.T) {
	c := NewQueuePacketConn(DummyAddr{}, 0)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, _, err := c.ReadFrom(make([]byte, 8)); err == nil {
		t.Fatal("ReadFrom after Close succeeded")
	}
	if _, err := c.WriteTo([]byte{1}, DummyAddr{}); err == nil {
		t.Fatal("WriteTo after Close succeeded")
	}
	c.QueueIncoming([]byte{1}, DummyAddr{}) // must not panic or block
	if err := c.Close(); err == nil {
		t.Fatal("second Close succeeded, want stored closed error")
	}
}

// TestSetQueueSizeBeforeCreate: sizing for the window ceiling up front (the
// -bbr 2048 path) applies to subsequently created queues.
func TestSetQueueSizeBeforeCreate(t *testing.T) {
	old := GetQueueSize()
	defer SetQueueSize(old)
	SetQueueSize(2048)
	c := NewQueuePacketConn(DummyAddr{}, 0)
	if got := cap(c.recvQueue); got != 2048 {
		t.Fatalf("recvQueue cap %d, want 2048", got)
	}
	if got := cap(c.OutgoingQueue(DummyAddr{})); got != 2048 {
		t.Fatalf("send queue cap %d, want 2048", got)
	}
}

// TestStashUnstashRoundTrip: stash holds one packet for the next bundle;
// a second stash while occupied is refused, never overwrites.
func TestStashUnstashRoundTrip(t *testing.T) {
	c := NewQueuePacketConn(DummyAddr{}, 0)
	addr := DummyAddr{}
	if !c.Stash([]byte{0x11}, addr) {
		t.Fatal("empty stash refused")
	}
	if c.Stash([]byte{0x22}, addr) {
		t.Fatal("occupied stash accepted a second packet (overwrite)")
	}
	select {
	case p := <-c.Unstash(addr):
		if len(p) != 1 || p[0] != 0x11 {
			t.Fatalf("unstashed %x, want first packet", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unstash blocked on occupied stash")
	}
}
