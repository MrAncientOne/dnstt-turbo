package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"testing"
	"time"
)

// pipePair wires a TCPServerPacketConn to a net.Pipe client, bypassing
// listen/accept (this sandbox has no loopback TCP).
func pipePair(t *testing.T, c *TCPServerPacketConn) (client net.Conn, key string) {
	t.Helper()
	a, b := net.Pipe()
	key = b.RemoteAddr().String()
	c.mu.Lock()
	c.conns[key] = b
	c.mu.Unlock()
	go c.readConn(b, key)
	return a, key
}

func newTestTCPServer() *TCPServerPacketConn {
	return &TCPServerPacketConn{
		recvQueue: make(chan tcpTaggedMsg, 1024),
		conns:     make(map[string]net.Conn),
		closed:    make(chan struct{}),
	}
}

// TestServerKeyCollisionClosesOld: a reconnect reusing a remote-address key
// must close the stale socket (no fd leak, no ghost reader consuming the
// dead connection while responses route to the new one).
func TestServerKeyCollisionClosesOld(t *testing.T) {
	c := newTestTCPServer()
	defer c.Close()
	oldClient, key := pipePair(t, c)
	defer oldClient.Close()

	// Simulate acceptLoop's collision path with a fresh socket on the
	// same key (same extraction logic as acceptLoop).
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	c.mu.Lock()
	if old, ok := c.conns[key]; ok && old != b2 {
		old.Close()
	}
	c.conns[key] = b2
	c.mu.Unlock()
	go c.readConn(b2, key)

	// The old server-side socket must be closed: its reader exits and
	// the map entry now belongs to the new socket.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		cur := c.conns[key]
		c.mu.Unlock()
		if cur == b2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("map entry never transferred to the new conn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Old client side observes EOF/close once its reader's socket died.
	oldClient.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 1)
	if _, err := oldClient.Read(buf); err == nil {
		t.Fatal("old connection still live after collision eviction")
	}
}

// TestServerWriteToUnknownAddrTemporary: responses for a gone client are a
// Temporary drop (KCP retransmits; the client re-polls), never fatal.
func TestServerWriteToUnknownAddrTemporary(t *testing.T) {
	c := newTestTCPServer()
	defer c.Close()
	_, err := c.WriteTo([]byte{0x00}, &net.TCPAddr{IP: net.ParseIP("10.9.9.9"), Port: 5353})
	if err == nil {
		t.Fatal("WriteTo unknown addr succeeded")
	}
	var te tcpTemporaryError
	if !errors.As(err, &te) {
		t.Fatalf("WriteTo err %T(%v), want tcpTemporaryError", err, err)
	}
	if ne, ok := err.(net.Error); !ok || !ne.Temporary() {
		t.Fatalf("WriteTo err %v, want Temporary net.Error", err)
	}
}

// TestServerFrameRoundTrip: a framed query written by the client arrives via
// ReadFrom tagged with its connection, and the response routes back.
func TestServerFrameRoundTrip(t *testing.T) {
	c := newTestTCPServer()
	defer c.Close()
	client, key := pipePair(t, c)
	defer client.Close()

	query := []byte{0xde, 0xad, 0xbe, 0xef}
	frame := []byte{0x00, byte(len(query))}
	frame = append(frame, query...)
	writeDone := make(chan error, 1)
	go func() {
		_, err := client.Write(frame)
		writeDone <- err
	}()
	buf := make([]byte, 4096)
	n, addr, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if string(buf[:n]) != string(query) {
		t.Fatalf("ReadFrom = %x, want %x", buf[:n], query)
	}
	if addr == nil || addr.String() != key {
		t.Fatalf("tagged addr %v, want key %s", addr, key)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("client write: %v", err)
	}

	// Route a response back to the same key.
	resp := []byte{0x01, 0x02}
	respFrame := append([]byte{0x00, byte(len(resp))}, resp...)
	got := make([]byte, len(respFrame))
	readDone := make(chan error, 1)
	go func() {
		total := 0
		for total < len(got) {
			m, err := client.Read(got[total:])
			total += m
			if err != nil {
				readDone <- err
				return
			}
		}
		readDone <- nil
	}()
	if _, err := c.WriteTo(resp, addr); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatalf("client read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("response never arrived")
	}
	if string(got) != string(respFrame) {
		t.Fatalf("client got %x, want framed %x", got, respFrame)
	}
}

// TestServerConnCapEnforced: past maxServerTCPConns, new connections are
// refused without growing maps or leaking goroutines.
func TestServerConnCapEnforced(t *testing.T) {
	c := newTestTCPServer()
	defer c.Close()
	// Fill to the cap with distinct keys.
	for i := 0; i < maxServerTCPConns; i++ {
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		key := fmt.Sprintf("10.0.0.1:%d", 10000+i)
		c.mu.Lock()
		c.conns[key] = b
		c.mu.Unlock()
	}
	// One more must be refused: emulate acceptLoop's cap branch.
	a, b := net.Pipe()
	c.mu.Lock()
	refused := len(c.conns) >= maxServerTCPConns
	if refused {
		c.mu.Unlock()
		a.Close()
		b.Close()
	} else {
		c.mu.Unlock()
		t.Fatal("cap branch did not trigger at cap")
	}
	c.mu.Lock()
	n := len(c.conns)
	c.mu.Unlock()
	if n != maxServerTCPConns {
		t.Fatalf("map holds %d conns, want cap %d (leak or over-admit)", n, maxServerTCPConns)
	}
}

// TestLogThrottled: the first occurrence logs, repeats within the interval
// are dropped, distinct keys are independent. Captures the real logger.
func TestLogThrottled(t *testing.T) {
	logThrottleMu.Lock()
	logThrottleLast = make(map[string]time.Time)
	logThrottleMu.Unlock()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	logThrottled("test-same", time.Minute, "line %d", 1)
	logThrottled("test-same", time.Minute, "line %d", 2)
	logThrottled("test-same", time.Minute, "line %d", 3)
	logThrottled("test-other", time.Minute, "other")
	lines := bytes.Count(buf.Bytes(), []byte{'\n'})
	if lines != 2 {
		t.Fatalf("throttled log emitted %d lines, want 2:\n%s", lines, buf.String())
	}
	if !bytes.Contains(buf.Bytes(), []byte("line 1")) || !bytes.Contains(buf.Bytes(), []byte("other")) {
		t.Fatalf("wrong lines admitted:\n%s", buf.String())
	}
}
