package main

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

var testBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// testDomain is the zone these tests serve.
func testDomain(t *testing.T) dns.Name {
	t.Helper()
	d, err := dns.ParseName("t.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// testQuery builds a wire-valid query carrying raw (already clientID-
// prefixed) payload bytes in the QNAME, advertising payloadSize.
func testQuery(t *testing.T, domain dns.Name, id uint16, raw []byte, payloadSize uint16) *dns.Message {
	t.Helper()
	enc := testBase32.EncodeToString(raw)
	var labels [][]byte
	for len(enc) > 0 {
		n := 63
		if len(enc) < n {
			n = len(enc)
		}
		labels = append(labels, []byte(enc[:n]))
		enc = enc[n:]
	}
	name, err := dns.NewName(append(labels, domain...))
	if err != nil {
		t.Fatal(err)
	}
	return &dns.Message{
		ID:    id,
		Flags: 0x0100,
		Question: []dns.Question{
			{Name: name, Type: dns.RRTypeTXT, Class: dns.ClassIN},
		},
		Additional: []dns.RR{
			{Name: dns.Name{}, Type: dns.RRTypeOPT, Class: payloadSize, Data: []byte{}},
		},
	}
}

// clientPayload wraps packets the way the client QNAME path does:
// clientID + zero padding + length-prefixed packets.
func clientPayload(id turbotunnel.ClientID, packets ...[]byte) []byte {
	var b bytes.Buffer
	b.Write(id[:])
	b.WriteByte(224) // zero padding bytes
	for _, p := range packets {
		b.WriteByte(byte(len(p)))
		b.Write(p)
	}
	return b.Bytes()
}

func TestResponseForHappyPath(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLIENT01"))
	q := testQuery(t, domain, 0x1234, clientPayload(cid, []byte("hi")), 4096)
	resp, payload, adv := responseFor(q, domain, 4096)
	if resp == nil {
		t.Fatal("valid query got no response")
	}
	if resp.ID != 0x1234 {
		t.Fatalf("response ID %x, want echo 0x1234", resp.ID)
	}
	if resp.Rcode() != dns.RcodeNoError {
		t.Fatalf("rcode %d, want NOERROR", resp.Rcode())
	}
	if adv != 4096 {
		t.Fatalf("advertised %d, want 4096", adv)
	}
	if string(payload[:8]) != "CLIENT01" {
		t.Fatalf("payload %q, want clientID prefix", payload)
	}
}

// TestResponseForMixedMTU is the regression test for the -mtu 16384 bug: a
// 4096-octet client must still get NOERROR from a 16384-octet serverMax, with
// per-response capping in sendLoop instead of FORMERR.
func TestResponseForMixedMTU(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	q := testQuery(t, domain, 2, clientPayload(cid, []byte("x")), 4096)
	resp, _, adv := responseFor(q, domain, 16384)
	if resp == nil || resp.Rcode() != dns.RcodeNoError {
		t.Fatalf("4096 client vs 16384 server rcode = %v, want NOERROR", resp)
	}
	if adv != 4096 {
		t.Fatalf("advertised %d, want 4096", adv)
	}
	// TCP-sized client against the same serverMax also passes.
	q = testQuery(t, domain, 3, clientPayload(cid, []byte("x")), 65535)
	resp, _, adv = responseFor(q, domain, 16384)
	if resp == nil || resp.Rcode() != dns.RcodeNoError {
		t.Fatalf("65535 client vs 16384 server rcode = %v, want NOERROR", resp)
	}
	if adv != 65535 {
		t.Fatalf("advertised %d, want 65535", adv)
	}
}

func TestResponseForRejects(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	good := func() *dns.Message {
		return testQuery(t, domain, 1, clientPayload(cid, []byte("x")), 4096)
	}
	// QR set: not a query at all.
	q := good()
	q.Flags = 0x8000
	if resp, _, _ := responseFor(q, domain, 4096); resp != nil {
		t.Fatal("QR response answered, want silence")
	}
	// Two OPT RRs: FORMERR.
	q = good()
	q.Additional = append(q.Additional, q.Additional[0])
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeFormatError {
		t.Fatalf("multi-OPT rcode = %v, want FORMERR", resp)
	}
	// EDNS version != 0: BADVERS.
	q = good()
	q.Additional[0].TTL = 1 << 16
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != (dns.ExtendedRcodeBadVers&0xf) {
		t.Fatalf("bad version rcode = %v, want BADVERS low bits", resp)
	}
	// Foreign domain: NXDOMAIN.
	q = good()
	other, _ := dns.ParseName("other.example.org")
	q.Question[0].Name = append(q.Question[0].Name[:len(q.Question[0].Name)-3], other...)
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeNameError {
		t.Fatalf("foreign domain rcode = %v, want NXDOMAIN", resp)
	}
	// Non-TXT qtype: NXDOMAIN.
	q = good()
	q.Question[0].Type = 1 // A
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeNameError {
		t.Fatalf("A qtype rcode = %v, want NXDOMAIN", resp)
	}
	// Garbage base32: NXDOMAIN, never a crash.
	q = good()
	q.Question[0].Name[0] = []byte("!!!not-base32!!!")
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeNameError {
		t.Fatalf("garbage base32 rcode = %v, want NXDOMAIN", resp)
	}
	// Below-protocol-minimum advertised payload: FORMERR.
	q = testQuery(t, domain, 1, clientPayload(cid, []byte("x")), 256)
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeFormatError {
		t.Fatalf("small payload-size rcode = %v, want FORMERR", resp)
	}
	// No OPT at all: FORMERR (no negotiated size to cap to).
	q = good()
	q.Additional = nil
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeFormatError {
		t.Fatalf("missing OPT rcode = %v, want FORMERR", resp)
	}
	// Protocol minimum is insufficient for this listener's 1400-byte KCP segment.
	q = testQuery(t, domain, 1, clientPayload(cid, []byte("x")), 512)
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeFormatError {
		t.Fatalf("512 payload-size rcode = %v, want FORMERR", resp)
	}
	// Zero or many questions: FORMERR.
	q = good()
	q.Question = nil
	if resp, _, _ := responseFor(q, domain, 4096); resp == nil || resp.Rcode() != dns.RcodeFormatError {
		t.Fatalf("empty question rcode = %v, want FORMERR", resp)
	}
}

func TestEdnsDataOptionEdges(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	// Present option round-trips exactly.
	q := testQuery(t, domain, 9, clientPayload(cid), 4096)
	opt := make([]byte, 4+3)
	binary.BigEndian.PutUint16(opt[0:2], ednsDataOptionCode)
	binary.BigEndian.PutUint16(opt[2:4], 3)
	copy(opt[4:], []byte("pkt"))
	q.Additional[0].Data = opt
	if got := ednsDataOption(q); string(got) != "pkt" {
		t.Fatalf("option = %q, want pkt", got)
	}
	// Absent option: nil.
	q2 := testQuery(t, domain, 9, clientPayload(cid), 4096)
	if got := ednsDataOption(q2); got != nil {
		t.Fatalf("absent option = %q, want nil", got)
	}
	// Truncated length field: ignored, never panics or over-reads.
	q3 := testQuery(t, domain, 9, clientPayload(cid), 4096)
	q3.Additional[0].Data = []byte{0xfd, 0xe9, 0x00, 0x40}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("truncated option panicked: %v", r)
			}
		}()
		if got := ednsDataOption(q3); got != nil {
			t.Fatalf("truncated option = %q, want nil", got)
		}
	}()
	// Wrong code skipped, ours later still found.
	q4 := testQuery(t, domain, 9, clientPayload(cid), 4096)
	other := []byte{0x00, 0x01, 0x00, 0x01, 0xAA}
	q4.Additional[0].Data = append(other, opt...)
	if got := ednsDataOption(q4); string(got) != "pkt" {
		t.Fatalf("multi-option = %q, want pkt", got)
	}
}

// fakePacketConn is a scripted net.PacketConn: canned reads, recorded writes.
type fakePacketConn struct {
	reads  [][]byte
	writes [][]byte
	raddr  net.Addr
	err    error // permanent read error once reads exhaust
}

func (f *fakePacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(f.reads) == 0 {
		if f.err != nil {
			return 0, nil, f.err
		}
		return 0, nil, errFakeClosed
	}
	n := copy(p, f.reads[0])
	f.reads = f.reads[1:]
	return n, f.raddr, nil
}

func (f *fakePacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	f.writes = append(f.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (f *fakePacketConn) Close() error                     { return nil }
func (f *fakePacketConn) LocalAddr() net.Addr              { return f.raddr }
func (f *fakePacketConn) SetDeadline(time.Time) error      { return nil }
func (f *fakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (f *fakePacketConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr2 struct{}

func (fakeAddr2) Network() string { return "fake" }
func (fakeAddr2) String() string  { return "fake-addr" }

var errFakeClosed = errFake("closed")

type errFake string

func (e errFake) Error() string   { return string(e) }
func (e errFake) Timeout() bool   { return false }
func (e errFake) Temporary() bool { return false }

// TestServerRecvLoopFeedsKCP: one wire query drives packets into ttConn and
// exactly one record onto ch; the EDNS upload packet rides along too.
func TestServerRecvLoopFeedsKCP(t *testing.T) {
	domain := testDomain(t)
	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLNT1234"))
	q := testQuery(t, domain, 0x55, clientPayload(cid, []byte("AB"), []byte("CDE")), 4096)
	// EDNS upload packet alongside the QNAME ones.
	opt := make([]byte, 4+2)
	binary.BigEndian.PutUint16(opt[0:2], ednsDataOptionCode)
	binary.BigEndian.PutUint16(opt[2:4], 2)
	copy(opt[4:], []byte("ED"))
	q.Additional[0].Data = opt
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
	case err := <-done:
		if err != errFakeClosed {
			t.Fatalf("recvLoop returned %v, want scripted close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recvLoop hung on one query")
	}
	select {
	case rec := <-ch:
		if rec.Resp == nil || rec.Resp.ID != 0x55 {
			t.Fatalf("record resp = %+v, want ID 0x55 echo", rec.Resp)
		}
	default:
		t.Fatal("no record emitted for a valid query")
	}
	// ttConn must hold AB, CDE (QNAME) and ED (EDNS option). Reads use a
	// timeout: a missing packet must fail the test, never hang the suite
	// (QueuePacketConn deadlines are unimplemented by design).
	for _, want := range []string{"AB", "CDE", "ED"} {
		if got := readTT(t, tt); got != want {
			t.Fatalf("ttConn packet = %q, want %q", got, want)
		}
	}
}

// readTT pulls one packet from ttConn with a hard timeout.
func readTT(t *testing.T, tt *turbotunnel.QueuePacketConn) string {
	t.Helper()
	type res struct {
		n   int
		err error
		buf []byte
	}
	done := make(chan res, 1)
	go func() {
		buf := make([]byte, 65536)
		n, _, err := tt.ReadFrom(buf)
		done <- res{n, err, buf}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ttConn read: %v", r.err)
		}
		return string(r.buf[:r.n])
	case <-time.After(5 * time.Second):
		t.Fatal("ttConn read hung: packet missing")
		return ""
	}
}

// TestServerRecvLoopShortID: a payload too short for a ClientID becomes
// NXDOMAIN (and still gets a response record), never a crash.
func TestServerRecvLoopShortID(t *testing.T) {
	domain := testDomain(t)
	q := testQuery(t, domain, 0x66, []byte("abc"), 4096) // 3 bytes < 8
	wire, err := q.WireFormat()
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakePacketConn{reads: [][]byte{wire}, raddr: fakeAddr2{}, err: errFakeClosed}
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	ch := make(chan *record, 100)
	done := make(chan error, 1)
	go func() { done <- recvLoop(domain, fc, tt, ch, 4096) }()
	<-done
	select {
	case rec := <-ch:
		if rec.Resp.Rcode() != dns.RcodeNameError {
			t.Fatalf("rcode = %d, want NXDOMAIN", rec.Resp.Rcode())
		}
	default:
		t.Fatal("no record for short-ID query")
	}
}

// TestServerSendLoopBundles: queued downstream packets are bundled
// length-prefixed into one TXT response; an unframeable packet is dropped,
// never panics the server.
func TestServerSendLoopBundles(t *testing.T) {
	oldDelay := maxResponseDelay
	maxResponseDelay = 5 * time.Millisecond
	defer func() { maxResponseDelay = oldDelay }()

	domain := testDomain(t)
	_ = domain
	var cid turbotunnel.ClientID
	copy(cid[:], []byte("CLNT1234"))
	tt := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	for _, p := range [][]byte{[]byte("one"), []byte("two"), []byte("three")} {
		if _, err := tt.WriteTo(p, cid); err != nil {
			t.Fatal(err)
		}
	}
	q := testQuery(t, testDomain(t), 0x77, clientPayload(cid), 4096)
	resp, _, adv := responseFor(q, testDomain(t), 4096)
	fc := &fakePacketConn{raddr: fakeAddr2{}}
	ch := make(chan *record, 10)
	ch <- &record{Resp: resp, Addr: fakeAddr2{}, ClientID: cid, Advertised: adv}
	close(ch)
	if err := sendLoop(fc, tt, ch, 4096, 4000); err != nil {
		t.Fatalf("sendLoop: %v", err)
	}
	if len(fc.writes) != 1 {
		t.Fatalf("%d responses written, want 1 bundle", len(fc.writes))
	}
	msg, err := dns.MessageFromWireFormat(fc.writes[0])
	if err != nil {
		t.Fatalf("response not parseable: %v", err)
	}
	if len(msg.Answer) != 1 {
		t.Fatalf("%d answers, want 1", len(msg.Answer))
	}
	payload, err := dns.DecodeRDataTXT(msg.Answer[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	// Expect one|two|three length-prefixed (2-byte big-endian framing).
	var got []string
	for len(payload) > 0 {
		if len(payload) < 2 {
			t.Fatalf("dangling framing in %x", payload)
		}
		ln := int(binary.BigEndian.Uint16(payload))
		payload = payload[2:]
		if len(payload) < ln {
			t.Fatalf("frame overruns payload")
		}
		got = append(got, string(payload[:ln]))
		payload = payload[ln:]
	}
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("bundle = %q, want [one two three]", got)
	}
}

// TestComputeMaxEncodedPayloadSane: the bundle budget is positive and well
// under the datagram limit for both UDP and TCP ceilings.
func TestComputeMaxEncodedPayloadSane(t *testing.T) {
	for _, limit := range []int{1232, 4096, 16384} {
		got := computeMaxEncodedPayload(limit)
		if got <= 0 || got >= limit {
			t.Fatalf("limit %d: budget %d, want in (0, %d)", limit, got, limit)
		}
	}
}
