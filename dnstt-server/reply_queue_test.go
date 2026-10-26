package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func TestReplyQueueBurstDoesNotCloseLiveCarrier(t *testing.T) {
	for _, size := range []int{1, 65535} {
		t.Run(fmt.Sprintf("payload_%d", size), func(t *testing.T) {
			c, peer, addr := replyPipe(t)
			payload := bytes.Repeat([]byte{7}, size)
			accepted := 0
			for i := 0; i < maxTCPReplyMessages+2; i++ {
				if _, err := c.WriteTo(payload, addr); err != nil {
					if ne, ok := err.(net.Error); !ok || !ne.Temporary() {
						t.Fatalf("queue admission must return a retryable error: %v", err)
					}
					break
				}
				accepted++
			}
			if accepted == maxTCPReplyMessages+2 {
				t.Fatal("queue did not enforce its limit")
			}
			peer.SetReadDeadline(time.Now().Add(time.Second))
			for i := 0; i < accepted; i++ {
				var n uint16
				if err := binary.Read(peer, binary.BigEndian, &n); err != nil {
					t.Fatalf("burst closed the live carrier before admitted reply %d: %v", i, err)
				}
				p := make([]byte, n)
				if _, err := io.ReadFull(peer, p); err != nil || !bytes.Equal(p, payload) {
					t.Fatalf("admitted reply lost or altered: %v", err)
				}
			}
			deadline := time.Now().Add(time.Second)
			for {
				_, err := c.WriteTo([]byte("alive"), addr)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("carrier did not resume: %v", err)
				}
				time.Sleep(time.Millisecond)
			}
			var n uint16
			if err := binary.Read(peer, binary.BigEndian, &n); err != nil {
				t.Fatal(err)
			}
			p := make([]byte, n)
			if _, err := io.ReadFull(peer, p); err != nil || string(p) != "alive" {
				t.Fatalf("carrier unusable after burst: %q %v", p, err)
			}
		})
	}
}

func replyPipe(t *testing.T) (*TCPServerPacketConn, net.Conn, net.Addr) {
	t.Helper()
	a, b := net.Pipe()
	c := &TCPServerPacketConn{conns: map[string]net.Conn{a.RemoteAddr().String(): a}, closed: make(chan struct{})}
	t.Cleanup(func() { c.Close(); b.Close() })
	return c, b, a.RemoteAddr()
}

func TestReplyQueueCopiesAndPreservesOrder(t *testing.T) {
	c, peer, addr := replyPipe(t)
	for i := 0; i < 20; i++ {
		p := []byte{byte(i)}
		if _, err := c.WriteTo(p, addr); err != nil {
			t.Fatal(err)
		}
		p[0] = 255 // caller owns its buffer again as soon as WriteTo returns
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	for i := 0; i < 20; i++ {
		var n uint16
		if err := binary.Read(peer, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(peer, p); err != nil {
			t.Fatal(err)
		}
		if len(p) != 1 || p[0] != byte(i) {
			t.Fatalf("reply %d: %v", i, p)
		}
	}
}

func TestReplyQueueOverflowAndCloseReleaseWorker(t *testing.T) {
	c, _, addr := replyPipe(t)
	overflow := false
	for i := 0; i < maxTCPReplyMessages+2; i++ {
		if _, err := c.WriteTo(make([]byte, 65535), addr); err != nil {
			overflow = true
			break
		}
	}
	if !overflow {
		t.Fatal("stalled peer accepted unlimited backlog")
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close leaked a blocked writer")
	}
}

func TestReplyWriterExitsOnReaderDisconnect(t *testing.T) {
	c := newTestTCPServer()
	defer c.Close()
	peer, key := pipePair(t, c)
	if _, err := c.WriteTo([]byte("reply"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	peer.Close()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		_, present := c.writers[key]
		c.mu.Unlock()
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnected reader retained idle writer")
		}
		time.Sleep(time.Millisecond)
	}
}
