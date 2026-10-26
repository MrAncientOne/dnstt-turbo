package fairqueue

import (
	"bytes"
	"testing"
	"time"
)

func TestReliableQueueWaitsThroughCongestion(t *testing.T) {
	cfg := testCfg()
	cfg.Reliable = true
	cfg.SubmitTimeout = 10 * time.Millisecond
	s := NewScheduler(cfg)
	defer s.Close()
	s.mu.Lock()
	s.bufferLimit = 1024
	s.mu.Unlock()
	writer := &scriptWriter{}
	f1, f2 := s.AddFlow(writer), s.AddFlow(writer)
	chunk := bytes.Repeat([]byte{9}, 1024)
	done := make(chan error, 2)
	go func() { done <- s.Submit(f1, chunk) }()
	for s.QueuedLen() != 1 {
		time.Sleep(time.Millisecond)
	}
	go func() { done <- s.Submit(f2, chunk) }()
	time.Sleep(40 * time.Millisecond)
	if st := s.Stats(); st.TimedOut != 0 || st.Dropped != 0 {
		t.Fatalf("congestion killed stream: %+v", st)
	}
	if s.QueuedBytes() > 1024 {
		t.Fatal("queue budget exceeded")
	}
	go s.Run()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("capacity waiter never resumed")
		}
	}
	if writer.totalWritten() != 2048 {
		t.Fatal("missing or duplicated bytes")
	}
}

func TestReliableQueueCloseReleasesCapacityWaiter(t *testing.T) {
	cfg := testCfg()
	cfg.Reliable = true
	s := NewScheduler(cfg)
	s.mu.Lock()
	s.bufferLimit = 1024
	s.mu.Unlock()
	f1, f2 := s.AddFlow(&scriptWriter{}), s.AddFlow(&scriptWriter{})
	done := make(chan error, 2)
	go func() { done <- s.Submit(f1, make([]byte, 1024)) }()
	for s.QueuedLen() != 1 {
		time.Sleep(time.Millisecond)
	}
	go func() { done <- s.Submit(f2, make([]byte, 1024)) }()
	s.Close()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != ErrFlowClosed {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown left submit blocked")
		}
	}
}
