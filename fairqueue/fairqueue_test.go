package fairqueue

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// smuxPairSeq unique-ifies unix socket paths across parallel tests.
var smuxPairSeq atomic.Uint64

// testCfg returns a fast, deterministic config for unit tests. No run loop
// is started unless the test starts one explicitly.
func testCfg() Config {
	return Config{
		Chunk:         1024,
		Idle:          50 * time.Millisecond,
		WriteTimeout:  20 * time.Millisecond,
		MaxBurst:      4,
		MaxSparse:     64,
		SubmitTimeout: 30 * time.Second, // tests override when probing timeouts
		Linger:        250 * time.Millisecond,
	}
}

// enqueue puts f in the given queue with a pending chunk, driving nextLocked
// deterministically without a run loop. Caller: same package, no lock held.
func enqueue(tb testing.TB, s *Scheduler, f *Flow, sparse bool, chunk []byte) {
	tb.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	f.pending = chunk
	f.ready = true
	f.inQueue = true
	if sparse {
		s.sparse = append(s.sparse, f)
	} else {
		s.ring = append(s.ring, f)
	}
}

// drain picks n times via nextLocked, returning the served flows in order.
func drain(s *Scheduler, n int) []*Flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Flow
	for i := 0; i < n; i++ {
		f, _ := s.nextLocked()
		if f == nil {
			break
		}
		out = append(out, f)
	}
	return out
}

// TestPolicyReserveBoundaries checks the burst cap across values: with a
// storm of fresh sparse arrivals, a ring flow must surface within MaxBurst
// picks; MaxBurst<=0 restores strict priority (ring waits for full drain).
func TestPolicyReserveBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		burst   int
		storm   int // fresh sparse arrivals before we stop looking
		wantMax int // latest acceptable pick index for the ring flow
		// strict (burst<=0) never serves bulk while the storm continues:
		// wantMax stays -1 and the loop must exhaust without serving.
		wantStarve bool
	}{
		{"burst1", 1, 10, 1, false},
		{"burst4", 4, 10, 4, false},
		{"burst8", 8, 12, 8, false},
		{"strict", 0, 6, -1, true},
		{"strictNeg", -1, 6, -1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.MaxBurst = tc.burst
			s := NewScheduler(cfg)
			bulk := s.AddFlow(nil)
			enqueue(t, s, bulk, false, []byte("bulk"))
			at := -1
			for i := 0; i < tc.storm; i++ {
				st := s.AddFlow(nil)
				enqueue(t, s, st, true, []byte("storm"))
				s.mu.Lock()
				f, _ := s.nextLocked()
				s.mu.Unlock()
				if f == bulk {
					at = i
					break
				}
			}
			if tc.wantStarve {
				if at != -1 {
					t.Fatalf("strict mode: bulk served at %d, want starvation while storm continues", at)
				}
				return
			}
			if at < 0 || at > tc.wantMax {
				t.Fatalf("bulk served at pick %d, want within %d", at, tc.wantMax)
			}
		})
	}
}

// TestPolicyRingOrder checks DRR order and rr wrap-around: 8 ring flows with
// one chunk each are served 0..7 exactly once.
func TestPolicyRingOrder(t *testing.T) {
	s := NewScheduler(testCfg())
	flows := make([]*Flow, 8)
	for i := range flows {
		flows[i] = s.AddFlow(nil)
		enqueue(t, s, flows[i], false, []byte{byte(i)})
	}
	got := drain(s, 8)
	if len(got) != 8 {
		t.Fatalf("served %d flows, want 8", len(got))
	}
	seen := map[*Flow]int{}
	for i, f := range got {
		if seen[f]++; seen[f] > 1 {
			t.Fatalf("flow served twice at position %d", i)
		}
	}
	// Second round after re-enqueue must restart at index 0 (wrap-around).
	for _, f := range flows {
		enqueue(t, s, f, false, []byte{9})
	}
	got2 := drain(s, 8)
	if len(got2) != 8 {
		t.Fatalf("second round served %d, want 8 (rr wrap broken)", len(got2))
	}
}

// TestPolicyEmptyAndSparseOnly covers the degenerate queues.
func TestPolicyEmptyAndSparseOnly(t *testing.T) {
	s := NewScheduler(testCfg())
	s.mu.Lock()
	f, c := s.nextLocked()
	s.mu.Unlock()
	if f != nil || c != nil {
		t.Fatal("empty scheduler must return nil")
	}
	// Sparse-only with empty ring: served regardless of burst counter state.
	a, b := s.AddFlow(nil), s.AddFlow(nil)
	enqueue(t, s, a, true, []byte("a"))
	enqueue(t, s, b, true, []byte("b"))
	got := drain(s, 5)
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatal("sparse-only queue must drain FIFO")
	}
}

// TestDemotionBoundaries: MaxSparse 0 sends everything to the ring;
// N caps sparse at N with the rest demoted.
func TestDemotionBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		maxSparse  int
		submits    int
		wantSparse int
		wantRing   int
	}{
		{"zeroMeansAllRing", 0, 3, 0, 3},
		{"one", 1, 3, 1, 2},
		{"room", 64, 3, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.MaxSparse = tc.maxSparse
			s := NewScheduler(cfg) // no run loop: nothing drains
			var wg sync.WaitGroup
			for i := 0; i < tc.submits; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					f := s.AddFlow(nil)
					_ = s.Submit(f, []byte("x")) // parks; released by Close
				}()
			}
			time.Sleep(200 * time.Millisecond) // all parked in queue
			s.mu.Lock()
			ns, nr := len(s.sparse), len(s.ring)
			s.mu.Unlock()
			if ns != tc.wantSparse || nr != tc.wantRing {
				t.Fatalf("sparse=%d ring=%d, want %d/%d", ns, nr, tc.wantSparse, tc.wantRing)
			}
			st := s.Stats()
			if tc.wantRing > 0 && tc.maxSparse > 0 && st.Demoted != uint64(tc.wantRing) {
				t.Fatalf("demoted=%d, want %d", st.Demoted, tc.wantRing)
			}
			s.Close()
			wg.Wait()
		})
	}
}

// TestSubmitTimeoutFailsFast: no writer, submit gives up with ErrBusy and
// leaves nothing queued. TimedOut counts every expiry.
func TestSubmitTimeoutFailsFast(t *testing.T) {
	cfg := testCfg()
	cfg.SubmitTimeout = 50 * time.Millisecond
	s := NewScheduler(cfg) // no run loop
	const n = 5
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { errs <- s.Submit(s.AddFlow(nil), []byte("c")) }()
	}
	deadline := time.After(5 * time.Second)
	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err != ErrBusy {
				t.Fatalf("submit %d returned %v, want ErrBusy", i, err)
			}
		case <-deadline:
			t.Fatal("submit never returned without a writer (unbounded stall)")
		}
	}
	if q := s.QueuedLen(); q != 0 {
		t.Fatalf("queued %d after timeouts, want 0", q)
	}
	if st := s.Stats(); st.TimedOut != n {
		t.Fatalf("timedOut=%d, want %d", st.TimedOut, n)
	}
}

// TestTeardownMatrix covers every teardown path and their combinations.
func TestTeardownMatrix(t *testing.T) {
	t.Run("submitAfterClose", func(t *testing.T) {
		s := NewScheduler(testCfg())
		s.Close()
		if err := s.Submit(s.AddFlow(nil), []byte("x")); err != ErrFlowClosed {
			t.Fatalf("got %v, want ErrFlowClosed", err)
		}
	})
	t.Run("doubleClose", func(t *testing.T) {
		s := NewScheduler(testCfg())
		s.Close()
		s.Close() // must not hang or panic
	})
	t.Run("doubleFinish", func(t *testing.T) {
		s := NewScheduler(testCfg())
		f := s.AddFlow(nil)
		s.Finish(f)
		s.Finish(f) // idempotent
		if q := s.QueuedLen(); q != 0 {
			t.Fatalf("queued %d, want 0", q)
		}
	})
	t.Run("finishNeverSubmitted", func(t *testing.T) {
		s := NewScheduler(testCfg())
		s.Finish(s.AddFlow(nil)) // must not hang or panic
	})
	t.Run("closeReleasesParked", func(t *testing.T) {
		cfg := testCfg()
		cfg.SubmitTimeout = 30 * time.Second // close, not timeout, must release
		s := NewScheduler(cfg)
		done := make(chan error, 1)
		go func() { done <- s.Submit(s.AddFlow(nil), []byte("x")) }()
		time.Sleep(100 * time.Millisecond)
		s.Close()
		select {
		case err := <-done:
			if err != ErrFlowClosed {
				t.Fatalf("got %v, want ErrFlowClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("parked submit leaked across Close")
		}
		if q := s.QueuedLen(); q != 0 {
			t.Fatalf("queued %d after close, want 0", q)
		}
	})
	t.Run("timeoutThenFinish", func(t *testing.T) {
		cfg := testCfg()
		cfg.SubmitTimeout = 30 * time.Millisecond
		s := NewScheduler(cfg)
		f := s.AddFlow(nil)
		if err := s.Submit(f, []byte("x")); err != ErrBusy {
			t.Fatalf("got %v, want ErrBusy", err)
		}
		s.Finish(f) // must tolerate the already-timed-out flow
		if q := s.QueuedLen(); q != 0 {
			t.Fatalf("queued %d, want 0", q)
		}
	})
}

// TestWriteDeadlineRequeues: when smux writes stall past their deadline, the
// remainder must be requeued and the flow must still complete intact — never
// torn down. Misclassifying smux.ErrTimeout (a plain sentinel, no Timeout()
// method) as fatal truncated large transfers in production (bench: 25MB
// downloads dying mid-stream while small fetches flew).
//
// The stall is forced deterministically: the 512KB payload exceeds smux v2's
// initial per-stream window, and the peer reads nothing for 500ms, so every
// write in that window misses its 5ms deadline. Then the peer drains and the
// flow must finish whole.
func TestWriteDeadlineRequeues(t *testing.T) {
	cfg := testCfg()
	cfg.WriteTimeout = 5 * time.Millisecond
	cfg.SubmitTimeout = 60 * time.Second // requeues, not timeouts, must win
	s := NewScheduler(cfg)
	go s.Run()
	defer s.Close()
	cs, ss := smuxPair(t)

	const size = 512 * 1024
	payload := bytes.Repeat([]byte("D"), size)

	gotCh := make(chan []byte, 1)
	go func() {
		st, err := ss.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		time.Sleep(500 * time.Millisecond) // stall every write past deadline
		// Read exactly size: ReadAll would wait for EOF, but EOF only comes
		// from the sender's deferred Close after we deliver. ReadFull breaks
		// that cycle.
		got := make([]byte, size)
		if _, err := io.ReadFull(st, got); err != nil {
			t.Errorf("peer read: %v", err)
			return
		}
		gotCh <- got
	}()

	st, err := cs.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := s.AddFlow(st)
	defer s.Finish(f)
	if err := s.Pump(f, bytes.NewReader(payload)); err != io.EOF {
		t.Fatalf("pump returned %v, want io.EOF (full delivery despite expiries)", err)
	}
	select {
	case got := <-gotCh:
		if !bytes.Equal(got, payload) {
			t.Fatalf("peer got %d bytes, want %d intact", len(got), size)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("peer never received the payload")
	}
}

// TestLingerWaitsForHotFlows: a recently-served flow waits out the window;
// an idle (or never-served) flow returns immediately; disabled linger waits
// for nothing.
func TestLingerWaitsForHotFlows(t *testing.T) {
	s := NewScheduler(testCfg())
	fresh := s.AddFlow(nil)
	fresh.lastServe.Store(time.Now().UnixNano())
	t0 := time.Now()
	s.Linger(fresh)
	if el := time.Since(t0); el < 200*time.Millisecond || el > 2*time.Second {
		t.Fatalf("hot flow lingered %v, want ~%v", el, testCfg().Linger)
	}

	idle := s.AddFlow(nil)
	idle.lastServe.Store(time.Now().Add(-time.Hour).UnixNano())
	t0 = time.Now()
	s.Linger(idle)
	if el := time.Since(t0); el > 50*time.Millisecond {
		t.Fatalf("idle flow lingered %v, want immediate", el)
	}

	never := s.AddFlow(nil) // zero lastServe
	t0 = time.Now()
	s.Linger(never)
	if el := time.Since(t0); el > 50*time.Millisecond {
		t.Fatalf("unserved flow lingered %v, want immediate", el)
	}

	s2 := NewScheduler(Config{Chunk: 1, Linger: 0})
	hot := s2.AddFlow(nil)
	hot.lastServe.Store(time.Now().UnixNano())
	t0 = time.Now()
	s2.Linger(hot)
	if el := time.Since(t0); el > 50*time.Millisecond {
		t.Fatalf("disabled linger waited %v, want immediate", el)
	}
}

// TestStalledWriterKeepsFlowsAlive: the production bulk-killer. When the
// writer picks a chunk up but the peer drains slower than SubmitTimeout
// (full KCP window on a saturated pipe), requeues must reset the timer:
// served-but-slow is not starvation. The old code failed every bulk flow
// with ErrBusy while 1-chunk small flows slipped through — exactly the
// live symptom (bulk dead, DNS alive).
//
// The 512KB payload exceeds smux v2's initial per-stream window, so with
// the peer reading nothing for 600ms (4x the 150ms timeout) the stall is
// real: every write in that window misses its deadline. Smaller payloads
// fit the window, never stall, and would pass vacuously.
func TestStalledWriterKeepsFlowsAlive(t *testing.T) {
	cfg := testCfg()
	cfg.WriteTimeout = 10 * time.Millisecond
	cfg.SubmitTimeout = 150 * time.Millisecond
	cfg.Linger = 0
	s := NewScheduler(cfg)
	go s.Run()
	defer s.Close()
	cs, ss := smuxPair(t)

	const size = 512 * 1024
	payload := bytes.Repeat([]byte("S"), size)
	gotCh := make(chan []byte, 1)
	go func() {
		st, err := ss.AcceptStream()
		if err != nil {
			return
		}
		defer st.Close()
		// Stall 4x past SubmitTimeout with zero reads: every write here
		// misses its deadline and requeues. Then drain fully.
		time.Sleep(600 * time.Millisecond)
		got := make([]byte, size)
		if _, err := io.ReadFull(st, got); err != nil {
			t.Errorf("peer read: %v", err)
			return
		}
		gotCh <- got
	}()

	st, err := cs.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := s.AddFlow(st)
	defer s.Finish(f)
	if err := s.Pump(f, bytes.NewReader(payload)); err != io.EOF {
		t.Fatalf("pump returned %v, want io.EOF (stall must not fail a served flow)", err)
	}
	select {
	case got := <-gotCh:
		if !bytes.Equal(got, payload) {
			t.Fatalf("peer got %d bytes, want %d intact", len(got), size)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("peer never received the payload")
	}
	if st := s.Stats(); st.TimedOut != 0 {
		t.Fatalf("timedOut=%d, want 0 (requeues must reset the timer)", st.TimedOut)
	}
}

// TestPumpEOF: a finite source is fully served and pump reports EOF.
func TestPumpEOF(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	cfg := smux.DefaultConfig()
	cfg.Version = 2
	ss, _ := smux.Server(server, cfg)
	defer ss.Close()
	cs, _ := smux.Client(client, cfg)
	defer cs.Close()

	st, err := cs.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	acceptCh := make(chan *smux.Stream, 1)
	go func() {
		a, err := ss.AcceptStream()
		if err == nil {
			acceptCh <- a
		}
	}()
	f := s.AddFlow(st)
	payload := bytes.Repeat([]byte("Z"), 10*1024)
	if err := s.Pump(f, bytes.NewReader(payload)); err != io.EOF {
		t.Fatalf("pump returned %v, want io.EOF", err)
	}
	s.Finish(f)
	st.Close()
	a := <-acceptCh
	got, err := io.ReadAll(a)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("peer got %d bytes err=%v, want full payload", len(got), err)
	}
}

// smuxPair builds a connected client/server smux pair over a unix-domain
// socket (real buffered sockets; this sandbox refuses TCP loopback dials).
// net.Pipe was tried first and REJECTED for this harness: its synchronous
// rendezvous plus thundering concurrent opens/closes produces spurious
// short tail reads (FIN processed while frames are still rendezvousing)
// that do not occur on any buffered transport. Throughput figures measured
// here are comparative (scheduler vs direct copy over the same transport),
// not absolute numbers.
func smuxPair(tb testing.TB) (cs, ss *smux.Session) {
	tb.Helper()
	path := fmt.Sprintf("/tmp/fqtest-%d-%d.sock", os.Getpid(), smuxPairSeq.Add(1))
	ln, err := net.Listen("unix", path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { ln.Close(); os.Remove(path) })
	type res struct {
		s   *smux.Session
		err error
	}
	cc := make(chan res, 1)
	go func() {
		c, err := net.Dial("unix", path)
		if err != nil {
			cc <- res{nil, err}
			return
		}
		s, err := smux.Client(c, smuxConfig())
		cc <- res{s, err}
	}()
	raw, err := ln.Accept()
	if err != nil {
		tb.Fatal(err)
	}
	ss, err = smux.Server(raw, smuxConfig())
	if err != nil {
		tb.Fatal(err)
	}
	r := <-cc
	if r.err != nil {
		tb.Fatal(r.err)
	}
	tb.Cleanup(func() { r.s.Close(); ss.Close() })
	return r.s, ss
}

func smuxConfig() *smux.Config {
	cfg := smux.DefaultConfig()
	cfg.Version = 2
	cfg.MaxStreamBuffer = 1 << 20
	return cfg
}

// TestIntegrityTCP: distinct payloads down many streams over real sockets
// arrive intact, in order per stream, with no cross-talk.
func TestIntegrityTCP(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	cs, ss := smuxPair(t)

	const nFlows = 8
	const size = 256 * 1024
	type result struct {
		i int
		b []byte
	}
	gotCh := make(chan result, nFlows)
	var rwg sync.WaitGroup
	for i := 0; i < nFlows; i++ {
		rwg.Add(1)
		go func(i int) {
			defer rwg.Done()
			st, err := cs.OpenStream()
			if err != nil {
				t.Errorf("OpenStream: %v", err)
				return
			}
			b, err := io.ReadAll(st)
			if err != nil {
				t.Errorf("read: %v", err)
			}
			gotCh <- result{i, b}
		}(i)
	}
	for i := 0; i < nFlows; i++ {
		st, err := ss.AcceptStream()
		if err != nil {
			t.Fatalf("AcceptStream: %v", err)
		}
		f := s.AddFlow(st)
		go func(f *Flow, st *smux.Stream, marker byte) {
			// Production close pattern: linger after clean EOF so the FIN
			// can't jump queued tail data in smux's priority send queue.
			if err := s.Pump(f, bytes.NewReader(bytes.Repeat([]byte{marker}, size))); err != io.EOF {
				t.Errorf("pump %q: %v", marker, err)
			} else {
				s.Linger(f)
			}
			s.Finish(f)
			st.Close()
		}(f, st, byte('A'+i))
	}
	done := make(chan struct{})
	go func() { rwg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("integrity run timed out")
	}
	seen := map[byte]int{}
	for i := 0; i < nFlows; i++ {
		r := <-gotCh
		if len(r.b) != size {
			t.Errorf("flow %d: %d bytes, want %d", r.i, len(r.b), size)
			continue
		}
		for j, c := range r.b {
			if c != r.b[0] {
				t.Fatalf("flow %d: corruption at %d", r.i, j)
			}
		}
		seen[r.b[0]]++
	}
	if len(seen) != nFlows {
		t.Fatalf("only %d distinct markers, want %d (cross-talk?)", len(seen), nFlows)
	}
}

// TestChurn2000: torrent-style churn, 2000 short flows open/pump/close.
// Must complete quickly with no goroutine growth afterwards.
func TestChurn2000(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	cs, ss := smuxPair(t)

	before := runtime.NumGoroutine()
	var swg sync.WaitGroup
	swg.Add(1)
	go func() {
		defer swg.Done()
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			go func(st *smux.Stream) {
				_, _ = io.Copy(io.Discard, st)
				st.Close()
			}(st)
		}
	}()

	const n = 2000
	errCh := make(chan error, n)
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := cs.OpenStream()
			if err != nil {
				errCh <- err
				return
			}
			defer st.Close()
			f := s.AddFlow(st)
			defer s.Finish(f)
			if err := s.Pump(f, bytes.NewReader([]byte(fmt.Sprintf("flow-%d-payload", i)))); err != nil && err != io.EOF {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("churn flow error: %v", err)
	}
	el := time.Since(start)
	t.Logf("2000 flows in %v (%.0f flows/s)", el, float64(n)/el.Seconds())
	if el > 60*time.Second {
		t.Fatalf("churn too slow: %v", el)
	}

	// Leak check: goroutines must settle back near baseline.
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > before+20 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if d := runtime.NumGoroutine() - before; d > 20 {
		t.Fatalf("goroutine growth +%d after churn (leak?)", d)
	}
	if q := s.QueuedLen(); q != 0 {
		t.Fatalf("queued %d after churn, want 0", q)
	}
}

// TestSparseLatencyUnderLoad: with bulk flows saturating the writer, a fresh
// sparse chunk must be served quickly (the interactive guarantee).
func TestSparseLatencyUnderLoad(t *testing.T) {
	cfg := testCfg()
	cfg.SubmitTimeout = 10 * time.Second
	s := NewScheduler(cfg)
	go s.Run()
	defer s.Close()
	cs, ss := smuxPair(t)

	// Drain side: accept everything, discard.
	go func() {
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			go func(st *smux.Stream) {
				_, _ = io.Copy(io.Discard, st)
				st.Close()
			}(st)
		}
	}()

	// Saturate with bulk: continuous large pumps (never sparse: idle < gap).
	stop := make(chan struct{})
	var bwg sync.WaitGroup
	for i := 0; i < 8; i++ {
		bwg.Add(1)
		go func() {
			defer bwg.Done()
			st, err := cs.OpenStream()
			if err != nil {
				return
			}
			defer st.Close()
			f := s.AddFlow(st)
			defer s.Finish(f)
			big := bytes.Repeat([]byte("B"), 1<<20)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := s.Pump(f, bytes.NewReader(big)); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(500 * time.Millisecond) // let saturation establish

	// Probe sparse latency 50 times.
	var worst time.Duration
	for i := 0; i < 50; i++ {
		st, err := cs.OpenStream()
		if err != nil {
			t.Fatalf("probe OpenStream: %v", err)
		}
		f := s.AddFlow(st)
		t0 := time.Now()
		if err := s.Submit(f, []byte("ping")); err != nil {
			st.Close()
			t.Fatalf("probe submit: %v", err)
		}
		if el := time.Since(t0); el > worst {
			worst = el
		}
		s.Finish(f)
		st.Close()
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	bwg.Wait()
	t.Logf("worst sparse wait under 8-flow saturation: %v", worst)
	if worst > 5*time.Second {
		t.Fatalf("sparse starved under load: worst wait %v", worst)
	}
	if st := s.Stats(); st.BulkTurns == 0 {
		t.Fatal("bulk never served during saturation (reserve broken)")
	}
}

// TestStatsAccounting: served turns add up; demotions and timeouts counted.
func TestStatsAccounting(t *testing.T) {
	cfg := testCfg()
	cfg.MaxSparse = 2
	cfg.SubmitTimeout = 30 * time.Second
	s := NewScheduler(cfg) // no run loop for the demotion half
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Submit(s.AddFlow(nil), []byte("x"))
		}()
	}
	time.Sleep(200 * time.Millisecond)
	st := s.Stats()
	if st.Demoted != 2 {
		t.Fatalf("demoted=%d, want 2", st.Demoted)
	}
	if q := s.QueuedLen(); q != 4 {
		t.Fatalf("queued=%d, want 4", q)
	}
	s.Close()
	wg.Wait()

	// Served-turn accounting with a live writer.
	s2 := NewScheduler(testCfg())
	go s2.Run()
	defer s2.Close()
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	scfg := smux.DefaultConfig()
	scfg.Version = 2
	ssv, _ := smux.Server(server, scfg)
	defer ssv.Close()
	cl, _ := smux.Client(client, scfg)
	defer cl.Close()
	go func() {
		for {
			a, err := ssv.AcceptStream()
			if err != nil {
				return
			}
			go func(a *smux.Stream) { _, _ = io.Copy(io.Discard, a) }(a)
		}
	}()
	st2, _ := cl.OpenStream()
	f := s2.AddFlow(st2)
	const chunks = 10
	for i := 0; i < chunks; i++ {
		if err := s2.Submit(f, []byte("0123456789abcdef")); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	s2.Finish(f)
	st2.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s2.Stats()
		if got.SparseTurns+got.BulkTurns >= chunks {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d turns served, want >= %d", got.SparseTurns+got.BulkTurns, chunks)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
