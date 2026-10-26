package main

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"
)

func TestAuditUploadRepliesDoNotConsumePollBudget(t *testing.T) {
	c := testDNSConn(testPathDomain(t), true)
	defer c.QueuePacketConn.Close()
	tr := &notifyingTransport{written: make(chan []byte, 100)}
	// Upload replies can be discarded by the server's bounded response queue.
	// Exercise actual send registration, not just a hand-crafted counter.
	for i := 0; i < pollLimit; i++ {
		if err := c.send(tr, kcpTestFrame(81, 100), captureAddr{}); err != nil {
			t.Fatal(err)
		}
		<-tr.written
	}
	c.pollChan <- struct{}{}
	done := make(chan error, 1)
	go func() { done <- c.sendLoop(tr, captureAddr{}) }()
	defer func() { c.QueuePacketConn.Close(); <-done }()
	select {
	case <-tr.written:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("upload response debt suppressed a ready download poll")
	}
}

type immediateReply struct {
	captureTransport
	c *DNSPacketConn
}

func (tr *immediateReply) WriteTo(p []byte, _ net.Addr) (int, error) {
	tr.c.mon.receivedByID(0, binary.BigEndian.Uint16(p))
	return len(p), nil
}
func TestAuditResponseBeforeWriteReturns(t *testing.T) {
	c := testDNSConn(testPathDomain(t), true)
	defer c.QueuePacketConn.Close()
	tr := &immediateReply{c: c}
	if err := c.send(tr, nil, captureAddr{}); err != nil {
		t.Fatal(err)
	}
	if n := c.mon.Inflight(); n != 0 {
		t.Fatalf("already answered query remains outstanding: %d", n)
	}
}

type failedWrite struct{ captureTransport }

func (tr *failedWrite) WriteTo([]byte, net.Addr) (int, error) { return 0, errors.New("carrier down") }
func TestFailedWriteReleasesQueryAndPoll(t *testing.T) {
	c := testDNSConn(testPathDomain(t), true)
	defer c.QueuePacketConn.Close()
	if err := c.send(&failedWrite{}, nil, captureAddr{}); err == nil {
		t.Fatal("failure disappeared")
	}
	if c.mon.Inflight() != 0 || c.mon.PollInflight() != 0 {
		t.Fatal("failed write occupied query or poll budget")
	}
}
func TestPollBudgetExpiresWithoutReusingLateReplyID(t *testing.T) {
	m, clock := testMonitor()
	m.sentQuery(100, 1, true)
	m.sentQuery(100, 2, false)
	if m.PollInflight() != 1 || m.Inflight() != 2 {
		t.Fatal("query kinds mixed")
	}
	clock.step(2 * time.Second)
	if m.PollInflight() != 0 || m.Inflight() != 2 {
		t.Fatal("orphan poll blocked recovery or lost its reserved ID")
	}
	m.receivedByID(100, 1)
	m.receivedByID(100, 2)
	if m.Inflight() != 0 {
		t.Fatal("late reply did not release query")
	}
}
