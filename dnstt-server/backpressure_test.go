package main

import (
	"errors"
	"net"
	"testing"
	"time"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

type reliableScript struct{ fakePacketConn }

func (c *reliableScript) Reliable() bool { return true }

func TestReliableResponseQueueBackpressureAndCancel(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserve", true: "cancel"}[cancel], func(t *testing.T) {
			domain := testDomain(t)
			var cid turbotunnel.ClientID
			query := testQuery(t, domain, 123, clientPayload(cid), 4096)
			wire, err := query.WireFormat()
			if err != nil {
				t.Fatal(err)
			}
			conn := &reliableScript{fakePacketConn{reads: [][]byte{wire}, raddr: fakeAddr2{}, err: errFakeClosed}}
			tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
			defer tt.Close()
			ch := make(chan *record)
			stop := make(chan struct{})
			done := make(chan error, 1)
			go func() { done <- recvLoopContext(domain, conn, tt, ch, 4096, stop) }()
			select {
			case err := <-done:
				t.Fatalf("full reliable queue discarded response: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if cancel {
				close(stop)
			} else {
				select {
				case rec := <-ch:
					if rec.Resp.ID != 123 {
						t.Fatal("wrong reply")
					}
				case <-time.After(time.Second):
					t.Fatal("reply missing")
				}
			}
			select {
			case err := <-done:
				if cancel && !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked enqueue ignored shutdown")
			}
		})
	}
}
