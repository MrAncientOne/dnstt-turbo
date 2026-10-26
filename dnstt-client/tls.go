package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

const dialTimeout = 30 * time.Second

// TLSPacketConn is a TLS- and TCP-based transport for DNS messages, used for
// DNS over TLS (DoT). Its WriteTo and ReadFrom methods exchange DNS messages
// over a TLS channel, prefixing each message with a two-octet length field as
// in DNS over TCP.
//
// TLSPacketConn deals only with already formatted DNS messages. It does not
// handle encoding information into the messages. That is rather the
// responsibility of DNSPacketConn.
//
// https://tools.ietf.org/html/rfc7858
type TLSPacketConn struct {
	mu     sync.Mutex
	conn   net.Conn
	cancel context.CancelFunc
	// QueuePacketConn is the direct receiver of ReadFrom and WriteTo calls.
	// recvLoop and sendLoop take the messages out of the receive and send
	// queues and actually put them on the network.
	*turbotunnel.QueuePacketConn
}

// NewTLSPacketConn creates a new TLSPacketConn configured to use the TLS
// server at addr as a DNS over TLS resolver. It maintains a TLS connection to
// the resolver, reconnecting as necessary. It closes the connection if any
// reconnection attempt fails.
func NewTLSPacketConn(addr string, dialTLSContext func(ctx context.Context, network, addr string) (net.Conn, error)) (*TLSPacketConn, error) {
	ctx, cancel := context.WithCancel(context.Background())
	dial := func() (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, dialTimeout)
		defer cancel()
		return dialTLSContext(ctx, "tcp", addr)
	}
	// We maintain one TLS connection at a time, redialing it whenever it
	// becomes disconnected. We do the first dial here, outside the
	// goroutine, so that any immediate and permanent connection errors are
	// reported directly to the caller of NewTLSPacketConn.
	conn, err := dial()
	if err != nil {
		cancel()
		return nil, err
	}
	c := &TLSPacketConn{
		conn: conn, cancel: cancel,
		QueuePacketConn: turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, 0),
	}
	go func() {
		defer c.Close()
		for {
			ended := make(chan struct{})
			var endOnce sync.Once
			end := func() { endOnce.Do(func() { close(ended); conn.Close() }) }
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				defer end()
				err := c.recvLoop(conn)
				if err != nil {
					log.Printf("recvLoop: %v", err)
				}
			}()
			go func() {
				defer wg.Done()
				defer end()
				err := c.sendLoop(conn, ended)
				if err != nil {
					log.Printf("sendLoop: %v", err)
				}
			}()
			wg.Wait()
			conn.Close()

			// Whenever the TLS connection dies, redial a new one.
			conn, err = dial()
			if err != nil {
				log.Printf("dial tls: %v", err)
				break
			}
			c.mu.Lock()
			select {
			case <-c.Done():
				c.mu.Unlock()
				conn.Close()
				return
			default:
			}
			c.conn = conn
			c.mu.Unlock()
		}
	}()
	return c, nil
}

// recvLoop reads length-prefixed messages from conn and passes them to the
// incoming queue.
func (c *TLSPacketConn) recvLoop(conn net.Conn) error {
	br := bufio.NewReader(conn)
	for {
		var length uint16
		err := binary.Read(br, binary.BigEndian, &length)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return err
		}
		p := make([]byte, int(length))
		_, err = io.ReadFull(br, p)
		if err != nil {
			return err
		}
		c.QueuePacketConn.QueueIncoming(p, turbotunnel.DummyAddr{})
	}
}

// sendLoop reads messages from the outgoing queue and writes them,
// length-prefixed, to conn.
func (c *TLSPacketConn) sendLoop(conn net.Conn, ended <-chan struct{}) error {
	bw := bufio.NewWriter(conn)
	for {
		var p []byte
		select {
		case <-c.Done():
			return net.ErrClosed
		case <-ended:
			return net.ErrClosed
		case p = <-c.QueuePacketConn.OutgoingQueue(turbotunnel.DummyAddr{}):
		}
		length := uint16(len(p))
		if int(length) != len(p) {
			// Cannot frame it; drop so one oversize packet can't kill
			// the transport loop (KCP retransmits in smaller pieces).
			log.Printf("sendLoop: dropping %d-byte packet, exceeds DNS-over-TLS framing", len(p))
			continue
		}
		err := binary.Write(bw, binary.BigEndian, &length)
		if err != nil {
			return err
		}
		_, err = bw.Write(p)
		if err != nil {
			return err
		}
		err = bw.Flush()
		if err != nil {
			return err
		}
	}
}

func (c *TLSPacketConn) Close() error {
	c.cancel()
	c.QueuePacketConn.Close()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
