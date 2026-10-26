package noise

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type failingNoiseConn struct {
	net.Conn
	fail  atomic.Bool
	short bool
}

func (c *failingNoiseConn) Write(p []byte) (int, error) {
	if c.fail.Load() {
		if c.short {
			return len(p) - 1, nil
		}
		return 0, io.ErrUnexpectedEOF
	}
	return c.Conn.Write(p)
}

func TestNoiseWriteFailurePermanentlyClosesSession(t *testing.T) {
	for _, short := range []bool{false, true} {
		a, b := net.Pipe()
		c := &failingNoiseConn{Conn: a, short: short}
		priv, err := GeneratePrivkey()
		if err != nil {
			t.Fatal(err)
		}
		peer := make(chan io.ReadWriteCloser, 1)
		go func() {
			s, e := NewServer(b, priv)
			if e == nil {
				peer <- s
			} else {
				peer <- nil
			}
		}()
		client, err := NewClient(c, PubkeyFromPrivkey(priv))
		if err != nil {
			t.Fatal(err)
		}
		server := <-peer
		if server == nil {
			t.Fatal("server handshake failed")
		}
		t.Cleanup(func() { client.Close(); server.Close(); a.Close(); b.Close() })
		c.fail.Store(true)
		if _, err := client.Write([]byte("record")); err == nil {
			t.Fatal("failed/short encrypted write succeeded")
		}
		c.fail.Store(false)
		done := make(chan error, 1)
		go func() { _, e := client.Write([]byte("must not reuse a desynchronized cipher")); done <- e }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("session reused after record failure")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("record failure did not close the unusable session")
		}
	}
}

func TestNoiseAuthenticationFailureClosesTransport(t *testing.T) {
	client, server := establishPair(t)
	defer client.Close()
	defer server.Close()
	// A complete, invalid ciphertext record exercises the actual decrypt path.
	_, err := server.(*socket).ReadWriteCloser.Write([]byte{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Read(make([]byte, 1))
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected integrity failure, got %v", err)
	}
	server.(*socket).ReadWriteCloser.(net.Conn).SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err = server.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("authentication failure left underlying transport open")
	}
}
