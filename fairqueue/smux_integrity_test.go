package fairqueue_test

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/fairqueue"
)

// Delay just the first PSH frame, preserving the real smux deadline logic.
type slowFirstData struct {
	net.Conn
	once  sync.Once
	delay time.Duration
}

func (s *slowFirstData) Write(p []byte) (int, error) {
	if len(p) > 1 && p[1] == 2 {
		s.once.Do(func() { time.Sleep(s.delay) })
	}
	return s.Conn.Write(p)
}

func streams(t *testing.T, delay time.Duration) (*smux.Stream, *smux.Stream) {
	t.Helper()
	a, b := net.Pipe()
	cfg := smux.DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	client, err := smux.Client(&slowFirstData{Conn: a, delay: delay}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	server, err := smux.Server(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	out, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	in, err := server.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	return out, in
}

func TestSmuxTimeoutPreservesBytes(t *testing.T) {
	out, in := streams(t, 100*time.Millisecond)
	cfg := fairqueue.DefaultConfig()
	cfg.WriteTimeout = 10 * time.Millisecond
	cfg.SubmitTimeout = time.Second
	s := fairqueue.NewScheduler(cfg)
	defer s.Close()
	go s.Run()
	err := s.Submit(s.AddFlow(out), []byte("hello"))
	in.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	got, _ := io.ReadAll(in)
	t.Logf("Submit error=%v; submitted=5 bytes; received=%d bytes: %q", err, len(got), got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Fatal("write deadline duplicated or lost payload")
	}
}

func TestDefaultSubmitTimeoutAllowsActiveWrite(t *testing.T) {
	out, in := streams(t, 300*time.Millisecond)
	s := fairqueue.NewScheduler(fairqueue.DefaultConfig())
	defer s.Close()
	go s.Run()
	start := time.Now()
	err := s.Submit(s.AddFlow(out), []byte("hello"))
	elapsed := time.Since(start)
	in.SetReadDeadline(time.Now().Add(time.Second))
	got := make([]byte, 5)
	_, readErr := io.ReadFull(in, got)
	t.Logf("Submit error=%v after %v; subsequent peer read=%q, error=%v", err, elapsed, got, readErr)
	if err != nil || readErr != nil || string(got) != "hello" {
		t.Fatal("active write must finish without a starvation error")
	}
}
