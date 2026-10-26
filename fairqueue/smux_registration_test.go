package fairqueue_test

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// The peer can receive SYN and send its first data before the local SYN Write
// returns. That is legal for a buffered socket and must not lose the response.
type delayedSYNReturn struct {
	net.Conn
	peerWrote <-chan struct{}
}

func (c delayedSYNReturn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && len(p) >= 8 && p[1] == 0 { // smux SYN
		<-c.peerWrote
		// Ensure the receive loop has processed the response before allowing
		// OpenStream to resume. This models a descheduled local writer.
		time.Sleep(10 * time.Millisecond)
	}
	return n, err
}

func TestSmuxRegistersStreamBeforeSendingSYN(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(time.Second))
	b.SetDeadline(time.Now().Add(time.Second))
	peerWrote := make(chan struct{})
	cfg := smux.DefaultConfig()
	cfg.Version = 2
	cfg.KeepAliveDisabled = true
	client, err := smux.Client(delayedSYNReturn{a, peerWrote}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := smux.Server(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peerErr := make(chan error, 1)
	go func() {
		defer close(peerWrote)
		st, err := server.AcceptStream()
		if err != nil {
			peerErr <- err
			return
		}
		_, err = st.Write([]byte("first response"))
		peerErr <- err
	}()
	st, err := client.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-peerErr; err != nil {
		t.Fatal(err)
	}
	st.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	got := make([]byte, len("first response"))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatalf("initial response lost before OpenStream returned: %v", err)
	}
	if string(got) != "first response" {
		t.Fatalf("corrupt response %q", got)
	}
}
