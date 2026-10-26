package main

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

// captureTransport is a scripted net.PacketConn: canned reads, recorded writes.
type captureTransport struct {
	reads  [][]byte
	writes [][]byte
	addr   net.Addr
	err    error
}

func (f *captureTransport) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(f.reads) == 0 {
		if f.err != nil {
			return 0, nil, f.err
		}
		return 0, nil, errCaptureClosed
	}
	n := copy(p, f.reads[0])
	f.reads = f.reads[1:]
	return n, f.addr, nil
}

func (f *captureTransport) WriteTo(p []byte, _ net.Addr) (int, error) {
	f.writes = append(f.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (f *captureTransport) Close() error                     { return nil }
func (f *captureTransport) LocalAddr() net.Addr              { return f.addr }
func (f *captureTransport) SetDeadline(time.Time) error      { return nil }
func (f *captureTransport) SetReadDeadline(time.Time) error  { return nil }
func (f *captureTransport) SetWriteDeadline(time.Time) error { return nil }

type captureAddr struct{}

func (captureAddr) Network() string { return "cap" }
func (captureAddr) String() string  { return "cap-addr" }

type errCapture string

func (e errCapture) Error() string   { return string(e) }
func (e errCapture) Timeout() bool   { return false }
func (e errCapture) Temporary() bool { return false }

var errCaptureClosed = errCapture("closed")

// testDNSConn builds a DNSPacketConn with a fixed client ID and no loops.
func testDNSConn(domain dns.Name, uploadInEDNS bool) *DNSPacketConn {
	var cid turbotunnel.ClientID
	copy(cid[:], []byte("TESTCLNT"))
	return &DNSPacketConn{
		clientID:          cid,
		domain:            domain,
		edns0PayloadClass: 65535,
		uploadInEDNS:      uploadInEDNS,
		pollChan:          make(chan struct{}, pollLimit),
		QueuePacketConn:   turbotunnel.NewQueuePacketConn(cid, 0),
		mon:               newBBRMonitor(),
	}
}

func testPathDomain(t *testing.T) dns.Name {
	t.Helper()
	d, err := dns.ParseName("t.example.com")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestNeedsPacingGate pins the bypass contract: nil pacer never paces;
// empty polls and control-only packets never pace; all upstream payload
// payload paces. Getting this wrong wedges the tunnel (live 0 B/s bulk
// when polls paced; stalled KCP when ACKs paced).
func TestNeedsPacingGate(t *testing.T) {
	c := testDNSConn(testPathDomain(t), true)
	if c.needsPacing(make([]byte, 1400)) {
		t.Fatal("nil pacer must never pace")
	}
	c.pacer = bbr.NewPacer(nil)
	c.pacer.SetRate(1, 1500) // 1 B/s: anything paced would take minutes
	for _, tc := range []struct {
		name string
		p    []byte
		want bool
	}{
		{"empty poll", nil, false},
		{"ACK batch", bytes.Repeat(kcpTestFrame(82, 0), 20), false},
		{"small QNAME upload", kcpTestFrame(81, 50), true},
		{"malformed", make([]byte, 300), false},
		{"bulk", kcpTestFrame(81, 1376), true},
	} {
		if got := c.needsPacing(tc.p); got != tc.want {
			t.Fatalf("%s: needsPacing=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSendQNAMEEncodes: QNAME-mode send emits a query whose decoded name
// carries our clientID and packet, registers the ID with the monitor, and
// counts wire bytes.
func TestSendQNAMEEncodes(t *testing.T) {
	domain := testPathDomain(t)
	c := testDNSConn(domain, false)
	tr := &captureTransport{addr: captureAddr{}}
	payload := []byte("payload-123")
	if err := c.send(tr, payload, captureAddr{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(tr.writes) != 1 {
		t.Fatalf("%d queries written, want 1", len(tr.writes))
	}
	msg, err := dns.MessageFromWireFormat(tr.writes[0])
	if err != nil {
		t.Fatalf("query not parseable: %v", err)
	}
	// The monitor must hold exactly this ID outstanding (exact-match RTT).
	c.mon.mu.Lock()
	outstanding := len(c.mon.pending[msg.ID]) == 1 && c.mon.pendingCount == 1
	sent := c.mon.sentBytes
	c.mon.mu.Unlock()
	if !outstanding {
		t.Fatal("sent query ID not registered with the monitor")
	}
	if sent != uint64(len(tr.writes[0])) {
		t.Fatalf("monitor sentBytes %d, want wire %d", sent, len(tr.writes[0]))
	}
	// Decode the QNAME back to clientID + packet.
	joined := bytes.Join(append([][]byte{}, msg.Question[0].Name[:len(msg.Question[0].Name)-3]...), nil)
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(string(bytes.ToUpper(joined)))
	if err != nil {
		t.Fatalf("qname not base32: %v", err)
	}
	if string(raw[:8]) != "TESTCLNT" {
		t.Fatalf("qname prefix %q, want clientID", raw[:8])
	}
}

// TestSendEDNSCarriesPacket: EDNS-mode send carries the packet in the
// private option (not the QNAME), keeping queries small while lifting the
// ~132 B upload cap.
func TestSendEDNSCarriesPacket(t *testing.T) {
	domain := testPathDomain(t)
	c := testDNSConn(domain, true)
	tr := &captureTransport{addr: captureAddr{}}
	payload := bytes.Repeat([]byte{'u'}, 1200) // past any QNAME cap
	if err := c.send(tr, payload, captureAddr{}); err != nil {
		t.Fatalf("send: %v", err)
	}
	msg, err := dns.MessageFromWireFormat(tr.writes[0])
	if err != nil {
		t.Fatal(err)
	}
	var found []byte
	for _, rr := range msg.Additional {
		if rr.Type != dns.RRTypeOPT {
			continue
		}
		data := rr.Data
		for len(data) >= 4 {
			code := binary.BigEndian.Uint16(data[0:2])
			ln := int(binary.BigEndian.Uint16(data[2:4]))
			if 4+ln > len(data) {
				break
			}
			if code == ednsDataOptionCode {
				found = data[4 : 4+ln]
			}
			data = data[4+ln:]
		}
	}
	if string(found) != string(payload) {
		t.Fatalf("EDNS option carries %d bytes, want full %d-byte packet", len(found), len(payload))
	}
}

// TestRecvLoopDispatches: a canned TXT response with two packets lands in
// the incoming queue byte-exact, counts delivery, matches the RTT by ID,
// and enqueues pollBurst polls.
func TestRecvLoopDispatches(t *testing.T) {
	domain := testPathDomain(t)
	c := testDNSConn(domain, true)
	// Register the query the response will answer.
	id := c.mon.reserveID()
	c.mon.sent(300, id)
	// Build the response: answer name must be a subdomain of domain.
	qname, err := dns.NewName(append([][]byte{[]byte("q")}, domain...))
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	for _, p := range [][]byte{[]byte("PKT1"), []byte("PKT22")} {
		binary.Write(&payload, binary.BigEndian, uint16(len(p)))
		payload.Write(p)
	}
	resp := &dns.Message{
		ID:    id,
		Flags: 0x8000,
		Question: []dns.Question{
			{Name: qname, Type: dns.RRTypeTXT, Class: dns.ClassIN},
		},
		Answer: []dns.RR{
			{Name: qname, Type: dns.RRTypeTXT, Class: dns.ClassIN, TTL: 60,
				Data: dns.EncodeRDataTXT(payload.Bytes())},
		},
	}
	wire, err := resp.WireFormat()
	if err != nil {
		t.Fatal(err)
	}
	tr := &captureTransport{reads: [][]byte{wire}, addr: captureAddr{}, err: errCaptureClosed}
	done := make(chan error, 1)
	go func() { done <- c.recvLoop(tr) }()
	select {
	case err := <-done:
		if err != errCaptureClosed {
			t.Fatalf("recvLoop = %v, want scripted close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recvLoop hung on one response")
	}
	// Both packets arrive whole.
	for _, want := range []string{"PKT1", "PKT22"} {
		buf := make([]byte, 64)
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatalf("incoming missing %q: %v", want, err)
		}
		if string(buf[:n]) != want {
			t.Fatalf("incoming = %q, want %q", buf[:n], want)
		}
	}
	// Monitor: 9 delivered bytes, ID matched (no longer outstanding).
	c.mon.mu.Lock()
	delivered := c.mon.delivered
	stillOut := len(c.mon.pending[id])
	c.mon.mu.Unlock()
	if delivered != 9 {
		t.Fatalf("monitor delivered %d, want 9", delivered)
	}
	if stillOut != 0 {
		t.Fatal("answered ID still outstanding")
	}
	// Poll burst: exactly pollBurst permits enqueued.
	if n := len(c.pollChan); n != pollBurst {
		t.Fatalf("pollChan holds %d, want pollBurst %d", n, pollBurst)
	}
}

// TestRecvLoopIgnoresGarbage: a corrupt datagram is skipped (logged), the
// loop keeps serving the next response.
func TestRecvLoopIgnoresGarbage(t *testing.T) {
	domain := testPathDomain(t)
	c := testDNSConn(domain, true)
	id := c.mon.reserveID()
	c.mon.sent(300, id)
	qname, _ := dns.NewName(append([][]byte{[]byte("q")}, domain...))
	var payload bytes.Buffer
	binary.Write(&payload, binary.BigEndian, uint16(2))
	payload.Write([]byte("OK"))
	resp := &dns.Message{ID: id, Flags: 0x8000,
		Question: []dns.Question{{Name: qname, Type: dns.RRTypeTXT, Class: dns.ClassIN}},
		Answer:   []dns.RR{{Name: qname, Type: dns.RRTypeTXT, Class: dns.ClassIN, Data: dns.EncodeRDataTXT(payload.Bytes())}},
	}
	wire, _ := resp.WireFormat()
	tr := &captureTransport{
		reads: [][]byte{{0xff, 0xff, 0x00}, wire}, // garbage, then valid
		addr:  captureAddr{},
		err:   errCaptureClosed,
	}
	done := make(chan error, 1)
	go func() { done <- c.recvLoop(tr) }()
	<-done
	buf := make([]byte, 64)
	n, _, err := c.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "OK" {
		t.Fatalf("after garbage: got %q err %v, want OK", buf[:n], err)
	}
}
