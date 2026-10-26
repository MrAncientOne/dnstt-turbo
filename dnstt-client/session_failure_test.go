package main

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/noise"
)

// This isolates a recovery gap, not the cause of the WAN upload stall.
// A real terminal write error must release the local listener and return the
// retryable error which main uses to rebuild KCP/Noise/smux.
type incidentObservedConn struct {
	net.Conn
	failed chan struct{}
	once   sync.Once
}

// Drive the real Noise decrypt failure -> smux close -> listener wakeup chain,
// with no application traffic needed to discover a dead protocol session.
func TestNoiseIntegrityFailureReleasesClientSession(t *testing.T) {
	for generation := 0; generation < 2; generation++ {
		a, b := net.Pipe()
		priv, err := noise.GeneratePrivkey()
		if err != nil {
			t.Fatal(err)
		}
		peer := make(chan io.ReadWriteCloser, 1)
		go func() {
			s, e := noise.NewServer(b, priv)
			if e == nil {
				peer <- s
			} else {
				peer <- nil
			}
		}()
		rw, err := noise.NewClient(a, noise.PubkeyFromPrivkey(priv))
		if err != nil {
			t.Fatal(err)
		}
		server := <-peer
		if server == nil {
			t.Fatal("server handshake failed")
		}
		cfg := smux.DefaultConfig()
		cfg.Version = 2
		cfg.KeepAliveDisabled = true
		sess, err := smux.Client(rw, cfg)
		if err != nil {
			t.Fatal(err)
		}
		closeSessionOn(sess, rw.(interface{ Done() <-chan struct{} }).Done())
		ln := &pipeListener{incoming: make(chan net.Conn), done: make(chan struct{})}
		done := make(chan error, 1)
		go func() { done <- serveLocalStreams(ln, sess, uint32(generation), nil) }()
		// Bypass server encryption to inject one invalid ciphertext record.
		if _, err := b.Write([]byte{0, 1, 0}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, errSessionLost) {
				t.Fatalf("not retryable: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("integrity error retained unusable client session")
		}
		sess.Close()
		rw.Close()
		server.Close()
		a.Close()
		b.Close()
	}
}

func (c *incidentObservedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.once.Do(func() { close(c.failed) })
	}
	return n, err
}

func TestIncidentTerminalSessionWriteFailureRestarts(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	observed := &incidentObservedConn{Conn: a, failed: make(chan struct{})}
	config := smux.DefaultConfig()
	config.Version = 2
	config.KeepAliveDisabled = true
	sess, err := smux.Client(observed, config)
	if err != nil {
		t.Fatal(err)
	}
	ln := &pipeListener{incoming: make(chan net.Conn, 1), done: make(chan struct{})}
	local, app := net.Pipe()
	defer app.Close()
	defer local.Close()
	done := make(chan error, 1)
	go func() { done <- serveLocalStreams(ln, sess, 1, nil) }()
	t.Cleanup(func() {
		sess.Close()
		ln.Close()
		observed.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("test cleanup stuck")
		}
	})
	// No reader on b: SYN's write reaches its real net.Pipe deadline.
	a.SetWriteDeadline(time.Now().Add(20 * time.Millisecond))
	ln.incoming <- halfClosePipe{local}
	select {
	case <-observed.failed:
	case <-time.After(time.Second):
		t.Fatal("did not trigger terminal write error")
	}
	select {
	case err := <-done:
		if !errors.Is(err, errSessionLost) {
			t.Fatalf("wrong recovery result: %v", err)
		}
		// Put it back for the cleanup join.
		done <- err
	case <-time.After(100 * time.Millisecond):
		t.Fatal("terminal write failed but local listener retained the unusable smux session")
	}
}
