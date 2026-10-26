package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// This file adds a plain DNS-over-TCP listener (-tcp) to dnstt-server, the
// counterpart of the client's -tcp transport. It presents a net.PacketConn:
// ReadFrom yields one whole DNS query (tagged with the accepting connection's
// address) and WriteTo routes a whole DNS response back over that same
// connection, each framed with the standard 2-byte big-endian length prefix.
// The turbotunnel/KCP session layer above is transport-agnostic, so run() drives
// it exactly as it does the UDP listener.

var errTCPConnGone = errors.New("tcp connection gone")

type tcpTemporaryError struct{ err error }

func (e tcpTemporaryError) Error() string   { return e.err.Error() }
func (e tcpTemporaryError) Timeout() bool   { return false }
func (e tcpTemporaryError) Temporary() bool { return true }

type tcpTaggedMsg struct {
	p    []byte
	addr net.Addr
}

// TCPServerPacketConn accepts DNS-over-TCP connections and exposes them as a
// single net.PacketConn. Each connection is identified by its remote address,
// which is used to route responses back to the connection a query arrived on.
type TCPServerPacketConn struct {
	ln            net.Listener
	recvQueue     chan tcpTaggedMsg
	mu            sync.Mutex
	conns         map[string]net.Conn
	writers       map[string]*tcpReplyWriter
	workers       sync.WaitGroup
	closed        chan struct{}
	closeOnce     sync.Once
	readDeadline  time.Time
	writeDeadline time.Time
}

// maxServerTCPConns caps concurrent accepted DNS-over-TCP connections.
// This is a single-client relay: one live connection is normal, a handful
// during reconnect overlap. Past the cap the newest connection is refused
// so a SYN/connection flood grows nothing (no map entries, no goroutines).
const maxServerTCPConns = 32

// A stalled reader owns only its socket and this bounded reply backlog.
// Include the in-progress write in the byte budget; DNS messages can be 64 KiB.
const maxTCPReplyBytes = 1 << 20
const maxTCPReplyMessages = 64

type tcpReplyWriter struct {
	conn     net.Conn
	queue    chan []byte
	done     chan struct{}
	stopOnce sync.Once
	bytes    int // guarded by TCPServerPacketConn.mu
}

func (w *tcpReplyWriter) stop() {
	w.stopOnce.Do(func() { close(w.done); w.conn.Close() })
}

// NewTCPServerPacketConn starts listening for DNS-over-TCP on address.
func NewTCPServerPacketConn(address string) (*TCPServerPacketConn, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	c := &TCPServerPacketConn{
		ln:        ln,
		recvQueue: make(chan tcpTaggedMsg, 1024),
		conns:     make(map[string]net.Conn),
		closed:    make(chan struct{}),
	}
	go c.acceptLoop()
	return c, nil
}

func (c *TCPServerPacketConn) acceptLoop() {
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			select {
			case <-c.closed:
				return
			default:
				if ne, ok := err.(net.Error); ok && ne.Temporary() {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				return
			}
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			// Dead-peer detection, mirroring the client: without
			// keepalive a blackholed client pins a goroutine plus map
			// entries here forever.
			tc.SetKeepAlive(true)
			tc.SetKeepAlivePeriod(30 * time.Second)
			if tcpSockBuf > 0 {
				if err := tc.SetReadBuffer(tcpSockBuf); err != nil {
					log.Printf("tcp SOCK_RCVBUF: %v", err)
				}
				if err := tc.SetWriteBuffer(tcpSockBuf); err != nil {
					log.Printf("tcp SOCK_SNDBUF: %v", err)
				}
			}
		}
		key := conn.RemoteAddr().String()
		c.mu.Lock()
		select {
		case <-c.closed:
			c.mu.Unlock()
			conn.Close()
			return
		default:
		}
		if old, ok := c.conns[key]; ok && old != conn {
			// Same key reconnected (NAT reuse / rapid redial): the old
			// socket's reader is still running and would keep consuming
			// a dead connection while responses route to the new one.
			// Close it now; its readConn cleans up (it only deletes the
			// map entry if it still owns the key).
			old.Close()
			if w := c.writers[key]; w != nil {
				w.stop()
			}
		}
		if len(c.conns) >= maxServerTCPConns {
			c.mu.Unlock()
			log.Printf("tcp: refusing %s: %d conns at cap", key, maxServerTCPConns)
			conn.Close()
			continue
		}
		c.conns[key] = conn
		c.mu.Unlock()
		go c.readConn(conn, key)
	}
}

func (c *TCPServerPacketConn) readConn(conn net.Conn, key string) {
	health.ActiveCarriers.Add(1)
	defer health.ActiveCarriers.Add(-1)
	log.Printf("role=server event=tcp_connected")
	defer log.Printf("role=server event=tcp_disconnected")
	defer func() {
		c.mu.Lock()
		if w := c.writers[key]; w != nil && w.conn == conn {
			w.stop()
		}
		if c.conns[key] == conn {
			delete(c.conns, key)
		}
		c.mu.Unlock()
		conn.Close()
	}()
	addr := conn.RemoteAddr()
	var lenbuf [2]byte
	for {
		c.mu.Lock()
		d := boundedTCPDeadline(c.readDeadline, 90*time.Second)
		conn.SetReadDeadline(d)
		c.mu.Unlock()
		if _, err := io.ReadFull(conn, lenbuf[:]); err != nil {
			health.Failure("tcp_read_header_failed", err)
			return
		}
		n := binary.BigEndian.Uint16(lenbuf[:])
		msg := make([]byte, n)
		if _, err := io.ReadFull(conn, msg); err != nil {
			health.Failure("tcp_read_body_failed", err)
			return
		}
		health.Received(len(msg))
		select {
		case c.recvQueue <- tcpTaggedMsg{p: msg, addr: addr}:
		case <-c.closed:
			return
		}
	}
}

// ReadFrom returns the next whole DNS query, tagged with its connection's addr.
func (c *TCPServerPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case m := <-c.recvQueue:
		return copy(p, m.p), m.addr, nil
	case <-c.closed:
		return 0, nil, net.ErrClosed
	}
}

// WriteTo copies and queues a reply for the connection identified by addr.
// Each socket has one writer, so a full socket cannot block the global DNS
// response loop. Queue exhaustion rejects only the unqueued datagram, so KCP
// can retransmit without resetting unrelated streams sharing this carrier.
// Success means queued, not delivered. A stuck socket still has a finite
// write deadline; this does not leave a non-reading peer blocked forever.
func (c *TCPServerPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if len(p) > 0xffff {
		return 0, errors.New("DNS-over-TCP message too long")
	}
	key := addr.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	conn := c.conns[key]
	if conn == nil {
		// Client likely reconnected on a new address; drop this
		// response. KCP will retransmit and the client re-polls.
		return 0, tcpTemporaryError{err: errTCPConnGone}
	}
	if c.writers == nil {
		c.writers = make(map[string]*tcpReplyWriter)
	}
	w := c.writers[key]
	if w == nil || w.conn != conn {
		if w != nil {
			w.stop()
		}
		w = &tcpReplyWriter{conn: conn, queue: make(chan []byte, maxTCPReplyMessages), done: make(chan struct{})}
		c.writers[key] = w
		c.workers.Add(1)
		go c.writeReplies(key, w)
	}
	select {
	case <-w.done:
		return 0, tcpTemporaryError{err: errTCPConnGone}
	default:
	}
	if w.bytes+len(p) > maxTCPReplyBytes || len(w.queue) == cap(w.queue) {
		// No part of this reply has entered the TCP byte stream. Dropping
		// this whole datagram preserves framing and every admitted reply.
		// A transient burst must not disconnect an otherwise live reader.
		return 0, tcpTemporaryError{err: errors.New("TCP reply queue full")}
	}
	w.bytes += len(p)
	w.queue <- append([]byte(nil), p...)
	return len(p), nil
}

func (c *TCPServerPacketConn) writeReplies(key string, w *tcpReplyWriter) {
	defer c.workers.Done()
	defer func() {
		w.stop()
		c.mu.Lock()
		if c.writers[key] == w {
			delete(c.writers, key)
		}
		if c.conns[key] == w.conn {
			delete(c.conns, key)
		}
		c.mu.Unlock()
	}()
	for {
		var p []byte
		select {
		case <-c.closed:
			return
		case <-w.done:
			return
		case p = <-w.queue:
		}
		c.mu.Lock()
		// A replaced key must never route queued old replies to the new peer.
		live := c.conns[key] == w.conn
		w.conn.SetWriteDeadline(boundedTCPDeadline(c.writeDeadline, 30*time.Second))
		c.mu.Unlock()
		if !live {
			return
		}
		var prefix [2]byte
		binary.BigEndian.PutUint16(prefix[:], uint16(len(p)))
		bufs := net.Buffers{prefix[:], p}
		if _, err := bufs.WriteTo(w.conn); err != nil {
			health.Failure("tcp_write_failed", err)
			return
		}
		health.TxFrames.Add(1)
		health.TxBytes.Add(uint64(len(p)))
		c.mu.Lock()
		w.bytes -= len(p)
		c.mu.Unlock()
	}
}

func (c *TCPServerPacketConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	// ln is always set by the constructor; the nil guard keeps Close
	// safe on partially constructed values (and in tests).
	if c.ln != nil {
		c.ln.Close()
	}
	c.mu.Lock()
	for _, conn := range c.conns {
		conn.Close()
	}
	for _, w := range c.writers {
		w.stop()
	}
	c.conns = make(map[string]net.Conn)
	c.mu.Unlock()
	c.workers.Wait()
	return nil
}

func (c *TCPServerPacketConn) LocalAddr() net.Addr { return c.ln.Addr() }

// Reliable permits the DNS receiver to apply bounded backpressure rather
// than discard replies when the response queue fills.
func (c *TCPServerPacketConn) Reliable() bool { return true }

// A finite I/O bound also detects blackholes whose TCP keepalive still works.
func boundedTCPDeadline(requested time.Time, limit time.Duration) time.Time {
	d := time.Now().Add(limit)
	if !requested.IsZero() && requested.Before(d) {
		return requested
	}
	return d
}
func (c *TCPServerPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	for _, conn := range c.conns {
		conn.SetReadDeadline(t)
	}
	return nil
}
func (c *TCPServerPacketConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	for _, conn := range c.conns {
		conn.SetWriteDeadline(t)
	}
	return nil
}
func (c *TCPServerPacketConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}
