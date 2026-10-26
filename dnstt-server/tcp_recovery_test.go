package main

import (
	"net"
	"testing"
	"time"
)

func TestTCPWriteDeadlineReleasesStalledPeer(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	addr := a.RemoteAddr()
	c := &TCPServerPacketConn{conns: map[string]net.Conn{addr.String(): a}, closed: make(chan struct{})}
	c.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	// WriteTo now acknowledges bounded queue admission. The socket worker
	// must still enforce the I/O deadline and close the stalled carrier.
	if _, err := c.WriteTo([]byte("response"), addr); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	w := c.writers[addr.String()]
	c.mu.Unlock()
	if w == nil {
		return // the deadline already closed and removed the worker
	}
	select {
	case <-w.done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("write deadline did not release the response writer")
	}
}
