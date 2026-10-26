package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// This file adds a plain DNS-over-TCP transport (-tcp) to dnstt-client. Unlike
// -doh/-dot it uses no TLS/HTTP: it opens a persistent TCP connection straight
// to the authoritative tunnel server on port 53 and frames each DNS message
// with the standard 2-byte big-endian length prefix (RFC 1035 §4.2.2). This is
// the transport that reaches a port-53 channel that heavily rate-limits/loses
// UDP but passes DNS-over-TCP with large responses (measured ~11 Mbps where UDP
// stalled). It reconnects automatically and reports transient failures as
// temporary net.Errors so DNSPacketConn's send/recv loops keep running.

var errNoTCPConn = errors.New("no live TCP connection")

// tcpTemporaryError wraps an error so it satisfies net.Error with
// Temporary()==true, keeping the DNSPacketConn loops alive across reconnects.
type tcpTemporaryError struct{ err error }

func (e tcpTemporaryError) Error() string   { return e.err.Error() }
func (e tcpTemporaryError) Timeout() bool   { return false }
func (e tcpTemporaryError) Temporary() bool { return true }

// TCPPacketConn is a net.PacketConn that carries whole DNS messages over one
// persistent, auto-reconnecting TCP connection using DNS-over-TCP framing.
type TCPPacketConn struct {
	remoteAddr    net.Addr
	address       string
	ctx           context.Context
	cancel        context.CancelFunc
	recvCh        chan []byte
	mu            sync.Mutex
	conn          net.Conn
	wmu           sync.Mutex
	closed        chan struct{}
	closeOnce     sync.Once
	readDeadline  time.Time
	writeDeadline time.Time
}

// NewTCPPacketConn dials (lazily, via the manager goroutine) a DNS-over-TCP
// server at address ("host:port").
func NewTCPPacketConn(address string) (*TCPPacketConn, error) {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &TCPPacketConn{
		ctx: ctx, cancel: cancel,
		remoteAddr: addr,
		address:    address,
		recvCh:     make(chan []byte, 256),
		closed:     make(chan struct{}),
	}
	go c.manage()
	return c, nil
}

// manage keeps exactly one live TCP connection up, redialing with backoff, and
// runs a framed read loop on it.
func (c *TCPPacketConn) manage() {
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		health.DialAttempts.Add(1)
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(c.ctx, "tcp", c.address)
		if err != nil {
			health.DialFailures.Add(1)
			health.Failure("tcp_dial_failed", err)
			select {
			case <-c.closed:
				return
			case <-time.After(backoff):
			}
			if backoff < 5*time.Second {
				backoff *= 2
			}
			continue
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			// Flush each query immediately; we are latency-sensitive
			// and never want Nagle to coalesce a query with the next.
			tc.SetNoDelay(true)
			// Dead-peer detection: without keepalive a blackholed
			// middlebox (buffered writes succeed, nothing arrives) wedges
			// this connection forever — readLoop blocks in ReadFull, the
			// manager never redials, and the tunnel stalls until process
			// restart. Keepalive turns a dead path into a read error
			// within a couple of minutes, and the manager redials.
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
			// Large kernel buffers absorb bursts between app reads so a
			// momentarily slow reader never stalls the pipe. 0 = OS default.
			if tcpSockBuf > 0 {
				if err := tc.SetReadBuffer(tcpSockBuf); err != nil {
					log.Printf("tcp SOCK_RCVBUF: %v", err)
				}
				if err := tc.SetWriteBuffer(tcpSockBuf); err != nil {
					log.Printf("tcp SOCK_SNDBUF: %v", err)
				}
			}
		}
		c.mu.Lock()
		select {
		case <-c.closed:
			c.mu.Unlock()
			conn.Close()
			return
		default:
		}
		c.conn = conn
		c.mu.Unlock()
		backoff = 100 * time.Millisecond
		health.ActiveCarriers.Add(1)
		log.Printf("role=client event=tcp_connected")
		c.readLoop(conn)
		health.ActiveCarriers.Add(-1)
		log.Printf("role=client event=tcp_disconnected")
		c.mu.Lock()
		if c.conn == conn {
			c.conn = nil
		}
		c.mu.Unlock()
		conn.Close()
	}
}

// readLoop reads length-prefixed DNS messages until the connection breaks.
func (c *TCPPacketConn) readLoop(conn net.Conn) {
	var lenbuf [2]byte
	br := conn
	for {
		c.mu.Lock()
		d := boundedTCPDeadline(c.readDeadline, 90*time.Second)
		conn.SetReadDeadline(d)
		c.mu.Unlock()
		if _, err := io.ReadFull(br, lenbuf[:]); err != nil {
			health.Failure("tcp_read_header_failed", err)
			return
		}
		n := binary.BigEndian.Uint16(lenbuf[:])
		msg := make([]byte, n)
		if _, err := io.ReadFull(br, msg); err != nil {
			health.Failure("tcp_read_body_failed", err)
			return
		}
		health.Received(len(msg))
		select {
		case c.recvCh <- msg:
		case <-c.closed:
			return
		}
	}
}

// ReadFrom returns the next whole DNS message received from the server.
func (c *TCPPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case msg := <-c.recvCh:
		return copy(p, msg), c.remoteAddr, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

// WriteTo sends p as one length-prefixed DNS message on the current connection.
func (c *TCPPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) > 0xffff {
		return 0, errors.New("DNS-over-TCP message too long")
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return 0, tcpTemporaryError{err: errNoTCPConn}
	}
	// Vectored write: length prefix + payload in one writev, no copy of p.
	var lp [2]byte
	binary.BigEndian.PutUint16(lp[:], uint16(len(p)))
	bufs := net.Buffers{lp[:], p}
	// A write mutex guards against interleaved frames if WriteTo is ever
	// called concurrently; net.Buffers.WriteTo completes before returning,
	// so the caller may reuse p afterwards.
	c.wmu.Lock()
	c.mu.Lock()
	conn.SetWriteDeadline(boundedTCPDeadline(c.writeDeadline, 30*time.Second))
	c.mu.Unlock()
	_, err := bufs.WriteTo(conn)
	c.wmu.Unlock()
	if err != nil {
		health.Failure("tcp_write_failed", err)
		conn.Close() // trigger a reconnect in manage()
		return 0, tcpTemporaryError{err: err}
	}
	health.TxFrames.Add(1)
	health.TxBytes.Add(uint64(len(p)))
	return len(p), nil
}

func (c *TCPPacketConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.cancel != nil {
			c.cancel()
		}
	})
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
	return nil
}

func (c *TCPPacketConn) LocalAddr() net.Addr {
	// The underlying connection redials, so prefer its live local
	// address and fall back to the configured remote (the pre-existing
	// behavior) when down.
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		if addr := conn.LocalAddr(); addr != nil {
			return addr
		}
	}
	return c.remoteAddr
}

// A finite I/O bound also detects blackholes whose TCP keepalive still works.
func boundedTCPDeadline(requested time.Time, limit time.Duration) time.Time {
	d := time.Now().Add(limit)
	if !requested.IsZero() && requested.Before(d) {
		return requested
	}
	return d
}
func (c *TCPPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	if c.conn != nil {
		return c.conn.SetReadDeadline(t)
	}
	return nil
}
func (c *TCPPacketConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	if c.conn != nil {
		return c.conn.SetWriteDeadline(t)
	}
	return nil
}
func (c *TCPPacketConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
