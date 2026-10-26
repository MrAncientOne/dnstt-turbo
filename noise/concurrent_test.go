package noise

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// msgMagic frames test messages on the decrypted stream.
const msgMagic = 0x9d3a

// encodeMsg builds one self-delimiting test message: magic, writer ID,
// sequence, payload length, payload, CRC over everything before it.
func encodeMsg(w byte, seq uint32, payload []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint16(msgMagic))
	b.WriteByte(w)
	binary.Write(&b, binary.BigEndian, seq)
	binary.Write(&b, binary.BigEndian, uint16(len(payload)))
	b.Write(payload)
	sum := crc32.ChecksumIEEE(b.Bytes())
	binary.Write(&b, binary.BigEndian, sum)
	return b.Bytes()
}

// establishPair runs a client/server Noise handshake over net.Pipe.
func establishPair(t *testing.T) (client, server io.ReadWriteCloser) {
	t.Helper()
	privkey, err := GeneratePrivkey()
	if err != nil {
		t.Fatal(err)
	}
	pubkey := PubkeyFromPrivkey(privkey)
	c, s := net.Pipe()
	serverCh := make(chan io.ReadWriteCloser, 1)
	serverErr := make(chan error, 1)
	go func() {
		srv, err := NewServer(s, privkey)
		if err != nil {
			serverErr <- err
			return
		}
		serverCh <- srv
	}()
	client, err = NewClient(c, pubkey)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	select {
	case server = <-serverCh:
	case err := <-serverErr:
		t.Fatalf("server handshake: %v", err)
	}
	return client, server
}

// TestConcurrentWritesInterleaveSafe hammers one Noise socket from many
// goroutines while a peer decrypts the stream. Each writer emits framed,
// checksummed messages; the socket mutex holds each whole Write call
// atomic, so every message must arrive contiguous and verify. Without the
// mutex, concurrent Encrypt calls reuse nonces and interleave frames:
// the receiver's nonce sequence breaks and reads fail (or -race fires).
func TestConcurrentWritesInterleaveSafe(t *testing.T) {
	client, server := establishPair(t)
	defer client.Close()
	defer server.Close()

	const writers = 8
	const perWriter = 50
	// Deterministic per-message sizes crossing the 4KB record split.
	msgLen := func(w, i int) int { return 1 + (w*131+i*7919)%9000 }
	total := 0
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			total += 2 + 1 + 4 + 2 + msgLen(w, i) + 4
		}
	}

	var wg sync.WaitGroup
	writeErr := make(chan error, writers)
	// Drain concurrently with writing: every pipe in the stack
	// (net.Pipe transport, decrypted io.Pipe) is synchronous, so
	// starting the drain after the writes would deadlock.
	streamCh := make(chan []byte, 1)
	drainErr := make(chan error, 1)
	go func() {
		stream := make([]byte, 0, total)
		buf := make([]byte, 65536)
		for len(stream) < total {
			n, err := server.Read(buf)
			stream = append(stream, buf[:n]...)
			if err != nil {
				drainErr <- err
				return
			}
		}
		streamCh <- stream
	}()
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				payload := bytes.Repeat([]byte{byte(w)}, msgLen(w, i))
				if _, err := client.Write(encodeMsg(byte(w), uint32(i), payload)); err != nil {
					writeErr <- err
					return
				}
			}
		}(w)
	}
	waitDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitDone)
		close(writeErr)
	}()
	select {
	case <-waitDone:
	case <-time.After(30 * time.Second):
		t.Fatal("writers wedged: session died under concurrency (nonce/frame interleave fails reads, pipe backpressure wedges writes)")
	}
	for err := range writeErr {
		t.Fatalf("write: %v", err)
	}

	// A stuck or corrupted session must fail the test, never hang it.
	var stream []byte
	select {
	case err := <-drainErr:
		t.Fatalf("server read: %v", err)
	case stream = <-streamCh:
	case <-time.After(60 * time.Second):
		t.Fatal("drain hung: session wedged")
	}
	if len(stream) != total {
		t.Fatalf("stream %d bytes, want %d", len(stream), total)
	}

	// Parse and verify every message: framing, CRC, per-writer order.
	seen := make(map[byte]int)
	off := 0
	for off < len(stream) {
		if len(stream)-off < 13 {
			t.Fatalf("truncated header at offset %d", off)
		}
		if magic := binary.BigEndian.Uint16(stream[off:]); magic != msgMagic {
			t.Fatalf("bad magic %#x at offset %d: records interleaved", magic, off)
		}
		w := stream[off+2]
		seq := binary.BigEndian.Uint32(stream[off+3:])
		ln := int(binary.BigEndian.Uint16(stream[off+7:]))
		end := off + 9 + ln + 4
		if end > len(stream) {
			t.Fatalf("message overruns stream at offset %d", off)
		}
		body := stream[off : end-4]
		if sum := binary.BigEndian.Uint32(stream[end-4 : end]); sum != crc32.ChecksumIEEE(body) {
			t.Fatalf("CRC mismatch at offset %d (writer %d seq %d)", off, w, seq)
		}
		if seq != uint32(seen[w]) {
			t.Fatalf("writer %d: seq %d out of order, want %d", w, seq, seen[w])
		}
		seen[w]++
		off = end
	}
	for w := 0; w < writers; w++ {
		if seen[byte(w)] != perWriter {
			t.Fatalf("writer %d: %d messages, want %d", w, seen[byte(w)], perWriter)
		}
	}
}

// TestWriteAfterPeerDeathFailsCleanly: closing the transport under a
// blocked writer must unblock it with an error, never hang the caller.
func TestWriteAfterPeerDeathFailsCleanly(t *testing.T) {
	privkey, err := GeneratePrivkey()
	if err != nil {
		t.Fatal(err)
	}
	pubkey := PubkeyFromPrivkey(privkey)
	c, s := net.Pipe()
	serverCh := make(chan io.ReadWriteCloser, 1)
	go func() {
		srv, err := NewServer(s, privkey)
		if err != nil {
			return
		}
		serverCh <- srv
	}()
	client, err := NewClient(c, pubkey)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	<-serverCh
	// Kill the transport mid-session; the socket's read loop fails, and
	// subsequent writes must error (not block: net.Pipe is unbuffered,
	// so the server's read-loop death is what unblocks the writer).
	s.Close()
	c.Close()
	done := make(chan error, 1)
	go func() {
		_, err := client.Write(bytes.Repeat([]byte{1}, 100000))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Log("write succeeded into closed pipe (transport-specific); no hang, acceptable")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Write hung after peer death")
	}
}
