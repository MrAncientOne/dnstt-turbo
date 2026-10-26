package main

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/noise"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

// shutdownConn blocks in ReadFrom until Close: the production shutdown path
// (dnsConn.Close unblocks recvLoop) in miniature. Before the fix, run()
// closed ch while recvLoop was still in ReadFrom, so the next query panicked
// with "send on closed channel".
type shutdownConn struct {
	closed chan struct{}
	once   chan struct{} // closed once via Close
	addr   net.Addr
}

func newShutdownConn() *shutdownConn {
	return &shutdownConn{closed: make(chan struct{}), once: make(chan struct{}), addr: fakeAddr2{}}
}

func (c *shutdownConn) ReadFrom(_ []byte) (int, net.Addr, error) {
	select {
	case <-c.closed:
		return 0, nil, net.ErrClosed
	case <-time.After(30 * time.Second):
		return 0, nil, net.ErrClosed
	}
}

func (c *shutdownConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }
func (c *shutdownConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}
func (c *shutdownConn) LocalAddr() net.Addr              { return c.addr }
func (c *shutdownConn) SetDeadline(time.Time) error      { return nil }
func (c *shutdownConn) SetReadDeadline(time.Time) error  { return nil }
func (c *shutdownConn) SetWriteDeadline(time.Time) error { return nil }

// TestShutdownOrderNoPanic: the fixed shutdown (Close conn, wait for recvLoop,
// then close ch) must never panic, even though the old
// select-default-on-closed-channel pattern panics by language semantics.
func TestShutdownOrderNoPanic(t *testing.T) {
	domain := testDomain(t)
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	sc := newShutdownConn()
	ch := make(chan *record, 100)
	done := make(chan error, 1)
	go func() { done <- recvLoop(domain, sc, tt, ch, 4096) }()
	time.Sleep(50 * time.Millisecond) // let recvLoop block in ReadFrom
	sc.Close()                        // production shutdown first step
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("recvLoop returned nil after Close, want net.ErrClosed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recvLoop did not return after Close")
	}
	// Only now is closing ch safe (no live sender). Must not panic.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("close(ch) after recvLoop exit panicked: %v", r)
			}
		}()
		close(ch)
	}()
}

// TestRunHalfDeadReturnsError: run() must return non-nil when its loops die,
// so main()'s supervised restart fires. The old code returned nil always.
func TestRunHalfDeadReturnsError(t *testing.T) {
	privkey, err := noise.GeneratePrivkey()
	if err != nil {
		t.Fatal(err)
	}
	domain := testDomain(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP loopback: %v", err)
	}
	// Pre-close: recvLoop fails immediately, forcing the half-dead path.
	pc.Close()
	done := make(chan error, 1)
	go func() { done <- run(privkey, domain, "127.0.0.1:9", pc, 1232) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run returned nil after loop death, want half-dead error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run hung after loop death (shutdown did not unblock stragglers)")
	}
}

// TestRunRejectsTinyCap: startup validation still rejects an unusable cap.
func TestRunRejectsTinyCap(t *testing.T) {
	privkey, err := noise.GeneratePrivkey()
	if err != nil {
		t.Fatal(err)
	}
	domain := testDomain(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP loopback: %v", err)
	}
	defer pc.Close()
	if err := run(privkey, domain, "127.0.0.1:9", pc, 100); err == nil {
		t.Fatal("run with cap 100 succeeded, want MTU error")
	}
}

// TestPerResponseCapSmallClient: serverMax 16384 with a 4096 client stays
// within 4096 wire bytes (the dual-listener fix).
func TestPerResponseCapSmallClient(t *testing.T) {
	oldDelay := maxResponseDelay
	maxResponseDelay = 5 * time.Millisecond
	defer func() { maxResponseDelay = oldDelay }()

	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLNT1234"))
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	big := make([]byte, 3000)
	for i := range big {
		big[i] = 'A'
	}
	if _, err := tt.WriteTo(big, cid); err != nil {
		t.Fatal(err)
	}
	q := testQuery(t, testDomain(t), 0x81, clientPayload(cid), 4096)
	resp, _, adv := responseFor(q, testDomain(t), 16384)
	if resp.Rcode() != dns.RcodeNoError {
		t.Fatalf("mixed-MTU query rejected: %v", resp.Rcode())
	}
	fc := &fakePacketConn{raddr: fakeAddr2{}}
	ch := make(chan *record, 10)
	ch <- &record{Resp: resp, Addr: fakeAddr2{}, ClientID: cid, Advertised: adv}
	close(ch)
	serverMax := 16384
	if err := sendLoop(fc, tt, ch, serverMax, computeMaxEncodedPayload(serverMax)); err != nil {
		t.Fatalf("sendLoop: %v", err)
	}
	if len(fc.writes) != 1 {
		t.Fatalf("%d writes, want 1", len(fc.writes))
	}
	if len(fc.writes[0]) > 4096 {
		t.Fatalf("small-client response %d bytes, want <= 4096", len(fc.writes[0]))
	}
}

// TestPerResponseCapLargeClient: the same serverMax lets a 65535 client use
// large responses (the download lever is preserved for TCP).
func TestPerResponseCapLargeClient(t *testing.T) {
	oldDelay := maxResponseDelay
	maxResponseDelay = 5 * time.Millisecond
	defer func() { maxResponseDelay = oldDelay }()

	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLNT1234"))
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	// Queue enough to fill a large bundle: 10 x 1200B.
	for i := 0; i < 10; i++ {
		p := make([]byte, 1200)
		for j := range p {
			p[j] = byte(i)
		}
		if _, err := tt.WriteTo(p, cid); err != nil {
			t.Fatal(err)
		}
	}
	q := testQuery(t, testDomain(t), 0x82, clientPayload(cid), 65535)
	resp, _, adv := responseFor(q, testDomain(t), 16384)
	fc := &fakePacketConn{raddr: fakeAddr2{}}
	ch := make(chan *record, 10)
	ch <- &record{Resp: resp, Addr: fakeAddr2{}, ClientID: cid, Advertised: adv}
	close(ch)
	serverMax := 16384
	if err := sendLoop(fc, tt, ch, serverMax, computeMaxEncodedPayload(serverMax)); err != nil {
		t.Fatalf("sendLoop: %v", err)
	}
	if len(fc.writes) != 1 {
		t.Fatalf("%d writes, want 1", len(fc.writes))
	}
	if len(fc.writes[0]) <= 4096 {
		t.Fatalf("large-client response %d bytes, want > 4096 (big-response lever)", len(fc.writes[0]))
	}
	if len(fc.writes[0]) > 16384 {
		t.Fatalf("large-client response %d bytes, want <= 16384", len(fc.writes[0]))
	}
}

// TestEdnsOversizeDropped: a >maxEDNSPacket upload option is dropped before
// KCP, while QNAME packets from the same query still land.
func TestEdnsOversizeDropped(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLNT1234"))
	q := testQuery(t, domain, 0x83, clientPayload(cid, []byte("OK")), 4096)
	huge := make([]byte, maxEDNSPacket+500)
	binary.BigEndian.PutUint16(huge[0:2], ednsDataOptionCode)
	binary.BigEndian.PutUint16(huge[2:4], uint16(maxEDNSPacket+500-4))
	q.Additional[0].Data = huge
	wire, err := q.WireFormat()
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakePacketConn{reads: [][]byte{wire}, raddr: fakeAddr2{}, err: errFakeClosed}
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	ch := make(chan *record, 100)
	done := make(chan error, 1)
	go func() { done <- recvLoop(domain, fc, tt, ch, 4096) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recvLoop hung on oversize EDNS query")
	}
	if got := readTT(t, tt); got != "OK" {
		t.Fatalf("QNAME packet = %q, want OK", got)
	}
	// No second packet: the oversize EDNS blob must not reach KCP.
	// QueuePacketConn.ReadFrom blocks, so probe with a timeout: a packet
	// arriving means the bound failed; a timeout means correctly dropped.
	type rr struct {
		n   int
		buf []byte
		err error
	}
	probe := make(chan rr, 1)
	go func() {
		buf := make([]byte, 65536)
		n, _, err := tt.ReadFrom(buf)
		probe <- rr{n, append([]byte(nil), buf[:n]...), err}
	}()
	select {
	case r := <-probe:
		if r.err == nil {
			t.Fatalf("oversize EDNS packet reached KCP: %q", r.buf[:r.n])
		}
	case <-time.After(200 * time.Millisecond):
	}
	// recvQueue should also be empty now (only OK was queued, already read).
	select {
	case rec := <-ch:
		if rec.Resp == nil {
			t.Fatal("nil response for oversize-EDNS query")
		}
	default:
		t.Fatal("no record for oversize-EDNS query")
	}
}
