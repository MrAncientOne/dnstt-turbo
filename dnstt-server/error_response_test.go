package main

import (
	"testing"

	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

func TestMalformedQueryCannotAbortResponseLoop(t *testing.T) {
	for _, advertised := range []uint16{1, 2, 255, 511, 512} {
		for _, questions := range []int{1, 100} {
			if advertised == 512 && questions == 1 {
				continue
			} // a supported valid query
			q := testQuery(t, testDomain(t), 1, clientPayload(turbotunnel.ClientID{}), advertised)
			first := q.Question[0]
			for len(q.Question) < questions {
				q.Question = append(q.Question, first)
			}
			resp, _, cap := responseFor(q, testDomain(t), 512)
			if resp.Rcode() != dns.RcodeFormatError {
				t.Fatalf("fixture did not produce FORMERR: cap=%d questions=%d", advertised, questions)
			}
			ch := make(chan *record, 1)
			ch <- &record{Resp: resp, Addr: fakeAddr2{}, Advertised: cap}
			close(ch)
			fc := &fakePacketConn{raddr: fakeAddr2{}}
			tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
			err := sendLoop(fc, tt, ch, 512, computeMaxEncodedPayload(512))
			tt.Close()
			if err != nil {
				t.Fatalf("remote cap=%d questions=%d aborted loop: %v", advertised, questions, err)
			}
			if len(fc.writes) != 1 || len(fc.writes[0]) > 512 {
				t.Fatal("missing or oversized error response")
			}
			if _, err := dns.MessageFromWireFormat(fc.writes[0]); err != nil {
				t.Fatalf("unparseable DNS error response: %v", err)
			}
		}
	}
}
