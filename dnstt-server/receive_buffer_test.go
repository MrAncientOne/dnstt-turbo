package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

// Responses outlive the read that created them. Reusing the receive buffer
// must preserve their names, IDs and the queued upstream payloads.
func TestReceiveBufferPreservesQueuedReplies(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	copy(cid[:], "CLIENT01")
	var wires [][]byte
	for i := 0; i < 32; i++ {
		q := testQuery(t, domain, uint16(i), clientPayload(cid, bytes.Repeat([]byte{byte(i)}, 100)), 4096)
		q.Additional[0].Data = make([]byte, 104)
		binary.BigEndian.PutUint16(q.Additional[0].Data[:2], ednsDataOptionCode)
		binary.BigEndian.PutUint16(q.Additional[0].Data[2:4], 100)
		copy(q.Additional[0].Data[4:], bytes.Repeat([]byte{byte(i + 32)}, 100))
		wire, err := q.WireFormat()
		if err != nil {
			t.Fatal(err)
		}
		wires = append(wires, wire)
	}
	c := &reliableScript{fakePacketConn{reads: wires, raddr: fakeAddr2{}, err: errFakeClosed}}
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	defer tt.Close()
	ch := make(chan *record, len(wires))
	if err := recvLoop(domain, c, tt, ch, 4096); !errors.Is(err, errFakeClosed) {
		t.Fatal(err)
	}
	for i := range wires {
		r := <-ch
		q := testQuery(t, domain, uint16(i), clientPayload(cid, bytes.Repeat([]byte{byte(i)}, 100)), 4096)
		if r.Resp.ID != uint16(i) || r.Resp.Question[0].Name.String() != q.Question[0].Name.String() {
			t.Fatalf("reply %d corrupted after subsequent reads", i)
		}
		for _, value := range []byte{byte(i), byte(i + 32)} {
			buf := make([]byte, 100)
			n, _, err := tt.ReadFrom(buf)
			if err != nil || n != 100 || !bytes.Equal(buf, bytes.Repeat([]byte{value}, 100)) {
				t.Fatalf("payload %d corrupted: n=%d err=%v", i, n, err)
			}
		}
	}
}

// A query's small parsed fields should not allocate a 64 KiB scratch buffer.
func TestReceiveQueryAllocationBudget(t *testing.T) {
	domain := testDomain(t)
	q := testQuery(t, domain, 1, clientPayload(turbotunnel.ClientID{}), 4096)
	wire, err := q.WireFormat()
	if err != nil {
		t.Fatal(err)
	}
	result := testing.Benchmark(func(b *testing.B) {
		wires := make([][]byte, b.N)
		for i := range wires {
			wires[i] = wire
		}
		conn := &fakePacketConn{reads: wires, raddr: fakeAddr2{}, err: errFakeClosed}
		tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
		defer tt.Close()
		ch := make(chan *record) // UDP replies may be discarded; parsing is identical.
		b.ResetTimer()
		if err := recvLoop(domain, conn, tt, ch, 4096); !errors.Is(err, errFakeClosed) {
			b.Fatal(err)
		}
	})
	t.Logf("receive query: %d B/op, %d allocs/op", result.AllocedBytesPerOp(), result.AllocsPerOp())
	if result.AllocedBytesPerOp() > 16384 {
		t.Fatalf("receive scratch allocation regression: %d B/query", result.AllocedBytesPerOp())
	}
}
