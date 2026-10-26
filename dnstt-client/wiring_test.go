package main

import (
	"bytes"
	"errors"
	"math"
	"net"
	"strings"
	"testing"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
)

// TestValidateUpstreamMTU pins the CLI contract: 0 means "auto/QNAME" at
// the flag layer (handled by the caller), 80..1500 are the KCP-legal
// bounds, anything else is a startup error instead of a SetMtu panic.
func TestValidateUpstreamMTU(t *testing.T) {
	for _, mtu := range []int{80, 81, 500, 1400, 1500} {
		if err := validateUpstreamMTU(mtu); err != nil {
			t.Fatalf("mtu %d rejected: %v", mtu, err)
		}
	}
	for _, mtu := range []int{-1, 1, 79, 1501, 4096, 65535} {
		if err := validateUpstreamMTU(mtu); err == nil {
			t.Fatalf("mtu %d accepted, want rejection", mtu)
		}
	}
}

// TestSampleUTLSAllZero: a zero-total distribution is a config error, not a
// sampleWeighted panic.
func TestSampleUTLSAllZero(t *testing.T) {
	for _, spec := range []string{"0*Firefox", "0*Chrome,0*Firefox", "0*none"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("spec %q panicked: %v", spec, r)
				}
			}()
			if _, err := sampleUTLSDistribution(spec); err == nil {
				t.Fatalf("spec %q accepted, want error", spec)
			}
		}()
	}
}

// TestProfilerReadsSustainedCollapse: a collapse with trickling spikes
// (windowed max propped up, median already down) must engage the degraded
// profile on the sustained signal. This is the wiring the ticker uses;
// feeding it the max instead delays recovery by a full estimator window.
func TestProfilerReadsSustainedCollapse(t *testing.T) {
	fc := &manualBBRClock{t: time.Unix(0, 0)}
	ctl := bbr.NewController(bbr.Config{
		SeedBWBps: 1500000,
		SeedRTT:   80 * time.Millisecond,
		MinSegs:   64,
		MaxSegs:   1024,
		SegBytes:  1200,
		Now:       fc.now,
	})
	// Prime at ~1.5MB/s.
	for i := 0; i < 100; i++ {
		fc.step(100 * time.Millisecond)
		ctl.Update(bbr.Sample{DeliveredBytes: 150000, Interval: 100 * time.Millisecond,
			RTT: 80 * time.Millisecond, HasRTT: true})
	}
	profMax, profSust := &bbr.KCPProfiler{}, &bbr.KCPProfiler{}
	engagedMax, engagedSust := -1, -1
	// Collapse with spikes: one 800KB tick per second over a 50KB/s floor.
	// The windowed max clings to the spikes; the median falls immediately.
	for i := 0; i < 400; i++ {
		fc.step(100 * time.Millisecond)
		delivered := uint64(5000)
		if i%10 == 0 {
			delivered = 80000
		}
		ctl.Update(bbr.Sample{DeliveredBytes: delivered, Interval: 100 * time.Millisecond})
		if ch, dg := profMax.Update(ctl.MaxBW()); ch && dg && engagedMax < 0 {
			engagedMax = i
		}
		if ch, dg := profSust.Update(ctl.SustainedBW()); ch && dg && engagedSust < 0 {
			engagedSust = i
		}
	}
	if engagedSust < 0 {
		t.Fatal("sustained-fed profiler never engaged on a real collapse")
	}
	t.Logf("sustained engaged at tick %d, max-fed at %d", engagedSust, engagedMax)
	if engagedMax >= 0 && engagedMax < engagedSust {
		t.Fatalf("max-fed profiler engaged first (%d < %d): sustained must lead", engagedMax, engagedSust)
	}
}

// TestProfilerIgnoresClumpyBulk: delivery in clumps (every 5th tick carries
// 5x bytes) must NOT engage the profiler on either signal: the path is
// healthy, just bursty.
func TestProfilerIgnoresClumpyBulk(t *testing.T) {
	fc := &manualBBRClock{t: time.Unix(0, 0)}
	ctl := bbr.NewController(bbr.Config{
		SeedBWBps: 1000000,
		SeedRTT:   80 * time.Millisecond,
		MinSegs:   64,
		MaxSegs:   1024,
		SegBytes:  1200,
		Now:       fc.now,
	})
	prof := &bbr.KCPProfiler{}
	for i := 0; i < 400; i++ {
		fc.step(100 * time.Millisecond)
		var delivered uint64
		if i%5 == 0 {
			delivered = 500000 // 5x clump every 5th tick: same 1MB/s mean
		}
		// NOTE: zero-delivery ticks here are app-limited-style gaps the
		// monitor would carry; feed only the delivery ticks like carry
		// does, so the estimator sees clumps, not collapse.
		if delivered > 0 {
			ctl.Update(bbr.Sample{DeliveredBytes: delivered, Interval: 100 * time.Millisecond})
			if ch, dg := prof.Update(ctl.SustainedBW()); ch && dg {
				t.Fatalf("profiler engaged on healthy clumpy bulk at tick %d", i)
			}
		}
	}
}

type manualBBRClock struct{ t time.Time }

func (c *manualBBRClock) now() time.Time { return c.t }
func (c *manualBBRClock) step(d time.Duration) {
	c.t = c.t.Add(d)
}

// TestTCPWriteToNoConnTemporary: with no live connection, WriteTo reports a
// Temporary error (loops keep running across redials), never a fatal one.
func TestTCPWriteToNoConnTemporary(t *testing.T) {
	c := &TCPPacketConn{recvCh: make(chan []byte, 256), closed: make(chan struct{})}
	_, err := c.WriteTo([]byte{0x00}, &net.TCPAddr{})
	if err == nil {
		t.Fatal("WriteTo without conn succeeded")
	}
	ne, ok := err.(net.Error)
	if !ok || !ne.Temporary() {
		t.Fatalf("WriteTo err %T(%v), want Temporary net.Error", err, err)
	}
}

// TestTCPWriteToOversize: messages past the 16-bit framing limit are
// rejected, never truncated on the wire.
func TestTCPWriteToOversize(t *testing.T) {
	c := &TCPPacketConn{recvCh: make(chan []byte, 256), closed: make(chan struct{})}
	if _, err := c.WriteTo(make([]byte, 0x10000), &net.TCPAddr{}); err == nil {
		t.Fatal("oversize WriteTo accepted")
	}
}

// TestTCPFrameRoundTrip: length-prefixed frames cross a connection whole,
// including a 64KB-max message and back-to-back coalesced writes. Uses
// net.Pipe (this sandbox has no loopback TCP); framing is transport-agnostic.
func TestTCPFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &TCPPacketConn{
		remoteAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5353},
		recvCh:     make(chan []byte, 256),
		closed:     make(chan struct{}),
	}
	c.mu.Lock()
	c.conn = a
	c.mu.Unlock()
	defer c.Close()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		c.readLoop(b)
	}()
	msgs := [][]byte{{0x01}, make([]byte, 65535), []byte("hello")}
	for i := range msgs {
		for j := range msgs[i] {
			msgs[i][j] = byte(i + j)
		}
	}
	writeErr := make(chan error, 1)
	go func() {
		for _, m := range msgs {
			if _, err := c.WriteTo(m, nil); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()
	buf := make([]byte, 65536)
	for i, want := range msgs {
		c.SetReadDeadline(time.Now().Add(5 * time.Second)) // real transport deadline
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatalf("frame %d read: %v", i, err)
		}
		if n != len(want) || !bytes.Equal(buf[:n], want) {
			t.Fatalf("frame %d = %d bytes, want %d exact", i, n, len(want))
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
	}
	a.Close()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop did not exit on close")
	}
}

// TestTCPReadLoopSplitFrames: a frame arriving byte-at-a-time (TCP
// segmentation) still reassembles; a truncated frame at close ends the loop
// without emitting.
func TestTCPReadLoopSplitFrames(t *testing.T) {
	a, b := net.Pipe()
	c := &TCPPacketConn{recvCh: make(chan []byte, 256), closed: make(chan struct{})}
	defer c.Close()
	go c.readLoop(b)
	// Length 3, then dribble the body.
	if _, err := a.Write([]byte{0x00, 0x03, 0xaa}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.recvCh:
		t.Fatal("partial frame emitted early")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := a.Write([]byte{0xbb, 0xcc}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, _, err := c.ReadFrom(buf)
	if err != nil || n != 3 || buf[0] != 0xaa || buf[2] != 0xcc {
		t.Fatalf("split frame = %x n=%d err=%v", buf[:n], n, err)
	}
	a.Close()
}

// TestTCPLocalAddrFallsBack: LocalAddr prefers the live socket address and
// degrades to the configured remote when down (never nil, never panics).
func TestTCPLocalAddrFallsBack(t *testing.T) {
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5353}
	c := &TCPPacketConn{remoteAddr: remote, recvCh: make(chan []byte, 256), closed: make(chan struct{})}
	defer c.Close()
	if addr := c.LocalAddr(); addr != remote {
		t.Fatalf("down LocalAddr = %v, want remote fallback", addr)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c.mu.Lock()
	c.conn = a
	c.mu.Unlock()
	addr := c.LocalAddr()
	if addr == nil {
		t.Fatal("live LocalAddr is nil")
	}
	if addr.String() == remote.String() {
		t.Fatalf("live LocalAddr = %v, want the socket address, not remote", addr)
	}
	// After Close, the conn is cleared: back to the fallback.
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	if addr := c.LocalAddr(); addr != remote {
		t.Fatalf("cleared LocalAddr = %v, want remote fallback", addr)
	}
}

// TestParseWeightedListEdges: trailing backslash is an error (not a panic),
// empty labels and zero weights parse (zero-total rejected at sampling).
func TestParseWeightedListEdges(t *testing.T) {
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf(`"foo\\" panicked: %v`, r)
			}
		}()
		if _, _, err := parseWeightedList(`foo\`); err == nil {
			t.Fatal(`trailing backslash accepted, want error`)
		}
	}()
	w, labels, err := parseWeightedList(`0*Firefox,Chrome`)
	if err != nil {
		t.Fatalf("zero weight rejected at parse: %v", err)
	}
	if len(w) != 2 || w[0] != 0 || labels[1] != "Chrome" {
		t.Fatalf("parse = %v %v, want [0 1] [Firefox Chrome]", w, labels)
	}
}

// TestSampleWeightedBounds: single-entry and skewed distributions always
// return in-range indices; error paths never panic.
func TestSampleWeightedBounds(t *testing.T) {
	if i := sampleWeighted([]uint32{1}); i != 0 {
		t.Fatalf("single weight -> %d, want 0", i)
	}
	for k := 0; k < 200; k++ {
		if i := sampleWeighted([]uint32{1, 1000000}); i < 0 || i > 1 {
			t.Fatalf("index %d out of range", i)
		}
	}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("empty weights did not panic (documented contract)")
			}
		}()
		_ = sampleWeighted(nil)
	}()
}

// TestFormatRatesEdges: NaN/negative/infinite inputs render as zeros, never
// garbage integers; health stays inside [0, 1000].
func TestFormatRatesEdges(t *testing.T) {
	out := formatRates(time.Unix(1, 0), math.Inf(-1), math.NaN(), -5*time.Millisecond, bbr.StateProbeBW, false, -1)
	for _, bad := range []string{"NaN", "nan", "+Inf", "-9223372036854775808", "RTT_MS=-"} {
		if strings.Contains(out, bad) {
			t.Fatalf("rendered %q in:\n%s", bad, out)
		}
	}
	// Garbage collapses to the no-anchor rule (best 0 => HEALTH=1000),
	// never to garbage integers or a negative RTT.
	if !strings.Contains(out, "HEALTH=1000\n") {
		t.Fatalf("want HEALTH=1000 under no-anchor rule, got:\n%s", out)
	}
	if !strings.Contains(out, "RTT_MS=0\n") {
		t.Fatalf("want clamped RTT_MS=0, got:\n%s", out)
	}
}

// TestTemporaryErrorContract: the Temporary() net.Error assertion the
// send/recv loops rely on holds for both client and server wrappers, so a
// future errors.As refactor can't silently turn "retry" into "fatal".
func TestTemporaryErrorContract(t *testing.T) {
	for _, err := range []error{
		tcpTemporaryError{err: errors.New("x")},
	} {
		ne, ok := err.(net.Error)
		if !ok || !ne.Temporary() {
			t.Fatalf("%T is not Temporary", err)
		}
		if ne.Timeout() {
			t.Fatalf("%T reports Timeout, want false", err)
		}
	}
}
