package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

func TestTCPWriteDeadlineReleasesStalledPeer(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	c := &TCPPacketConn{conn: a, closed: make(chan struct{})}
	c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	done := make(chan error, 1)
	go func() { _, err := c.WriteTo([]byte("query"), a.RemoteAddr()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("stalled write succeeded")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("write deadline did not release the query writer")
	}
}

func TestSessionDeathReleasesAdmissionAndLocalConnections(t *testing.T) {
	old := maxStreams
	maxStreams = 1
	defer func() { maxStreams = old }()
	// Exercise a fresh session twice: a dead session must not be reused.
	for generation := 0; generation < 2; generation++ {
		a, b := net.Pipe()
		cfg := smux.DefaultConfig()
		cfg.Version = 2
		cfg.KeepAliveDisabled = true
		sess, err := smux.Client(a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		peer, err := smux.Server(b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		ln := &pipeListener{incoming: make(chan net.Conn, 1), done: make(chan struct{})}
		t.Cleanup(func() { sess.Close(); peer.Close(); ln.Close() })
		done := make(chan error, 1)
		go func() { done <- serveLocalStreams(ln, sess, 1, nil) }()
		accepted, local := net.Pipe()
		defer local.Close()
		ln.incoming <- halfClosePipe{accepted}
		stream, err := peer.AcceptStream()
		if err != nil {
			t.Fatal(err)
		}
		local.SetDeadline(time.Now().Add(2 * time.Second))
		// Synchronize application traffic: with zero-latency net.Pipe,
		// smux's SYN can arrive before OpenStream registers its local stream.
		go local.Write([]byte("ready"))
		stream.SetReadDeadline(time.Now().Add(2 * time.Second))
		ready := make([]byte, 5)
		if _, err := io.ReadFull(stream, ready); err != nil {
			t.Fatal(err)
		}
		go stream.Write([]byte("fresh session"))
		got := make([]byte, len("fresh session"))
		if _, err := io.ReadFull(local, got); err != nil || !bytes.Equal(got, []byte("fresh session")) {
			t.Fatalf("generation %d: data=%q err=%v", generation, got, err)
		}
		// No more local writes: the local upload reader is parked, while
		// the accept loop is waiting on the full admission semaphore.
		sess.Close()
		select {
		case err := <-done:
			if !errors.Is(err, errSessionLost) {
				t.Fatalf("expected retryable session failure: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("session death left accept/handler blocked")
		}
		if _, err := local.Read(got); err == nil {
			t.Fatal("old local socket still open")
		}
		local.Close()
		peer.Close()
	}
}

func TestDNSCloseStopsWorkersAndTransport(t *testing.T) {
	transport := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	c := NewDNSPacketConn(transport, turbotunnel.DummyAddr{}, testPathDomain(t), 4096, false)
	c.Close()
	done := make(chan struct{})
	go func() { c.workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DNS workers leaked")
	}
	select {
	case <-transport.Done():
	default:
		t.Fatal("underlying transport left open")
	}
}

func TestMonitorHousekeepingWithoutBBR(t *testing.T) {
	now := time.Unix(0, 0)
	m := newBBRMonitor()
	m.now = func() time.Time { return now }
	for i := 0; i < 100000; i++ {
		m.sent(500, uint16(i))
		m.receivedByID(24, uint16(i))
		if i%1000 == 0 {
			m.prune()
		}
	}
	m.prune()
	if len(m.order) > 8*maxMonitorInflight {
		t.Fatalf("unbounded query history: %d", len(m.order))
	}
	for i := 0; i < 24; i++ {
		m.sent(100, uint16(i))
	}
	now = now.Add(monitorEntryTTL + time.Second)
	m.prune()
	if m.Inflight() != 0 {
		t.Fatal("lost queries still occupy poll budget")
	}
	if m.delivered == 0 {
		t.Fatal("housekeeping consumed estimator samples")
	}
}

// OS sockets are unavailable in the sandbox. This listener still exercises
// the production accept/admission/handler lifecycle with real smux sessions.
type pipeListener struct {
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return turbotunnel.DummyAddr{} }

type halfClosePipe struct{ net.Conn }

func (p halfClosePipe) CloseRead() error  { return nil }
func (p halfClosePipe) CloseWrite() error { return nil }

type blockingRoundTripper struct {
	started  chan struct{}
	canceled chan struct{}
}

func (rt blockingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	close(rt.started)
	<-r.Context().Done()
	close(rt.canceled)
	return nil, r.Context().Err()
}

func TestHTTPCloseCancelsPendingRequest(t *testing.T) {
	rt := blockingRoundTripper{make(chan struct{}), make(chan struct{})}
	c, err := NewHTTPPacketConn(rt, "https://example.invalid/dns-query", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.WriteTo([]byte("query"), turbotunnel.DummyAddr{})
	select {
	case <-rt.started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	c.Close()
	select {
	case <-rt.canceled:
	case <-time.After(time.Second):
		t.Fatal("old HTTP request not canceled")
	}
}

func TestTLSReconnectAndCloseReleaseWorkers(t *testing.T) {
	a1, b1 := net.Pipe()
	a2, b2 := net.Pipe()
	defer b1.Close()
	defer b2.Close()
	connections := make(chan net.Conn, 2)
	connections <- a1
	connections <- a2
	c, err := NewTLSPacketConn("unused", func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case conn := <-connections:
			return conn, nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A read-side failure must wake the sender even with no queued writes.
	b1.Close()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		connected := c.conn == a2
		c.mu.Unlock()
		if connected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TLS manager did not reconnect after idle read failure")
		}
		time.Sleep(time.Millisecond)
	}
	c.WriteTo([]byte("fresh"), turbotunnel.DummyAddr{})
	b2.SetReadDeadline(time.Now().Add(time.Second))
	var n uint16
	if err := binary.Read(b2, binary.BigEndian, &n); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(b2, got); err != nil || string(got) != "fresh" {
		t.Fatalf("reconnected payload=%q err=%v", got, err)
	}
	c.Close()
	if _, err := b2.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("closed TLS socket: %v", err)
	}
}

func TestStaticDNSExpiresLostQueries(t *testing.T) {
	if bbrOn {
		t.Fatal("test must exercise BBR-disabled operation")
	}
	transport := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0)
	c := NewDNSPacketConn(transport, turbotunnel.DummyAddr{}, testPathDomain(t), 4096, false)
	defer func() { c.Close(); c.workers.Wait() }()
	c.mon.mu.Lock()
	c.mon.pending[65000] = []time.Time{time.Now().Add(-2 * monitorEntryTTL)}
	c.mon.pendingCount++
	c.mon.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mon.mu.Lock()
		expired := len(c.mon.pending[65000]) == 0
		c.mon.mu.Unlock()
		if expired {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("BBR-disabled connection did not expire a lost query")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
