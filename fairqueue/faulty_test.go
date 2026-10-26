package fairqueue

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// scriptWriter is a StreamWriter that replays scripted per-call results,
// recording everything written. A step with full=true writes everything;
// otherwise it writes n bytes with err.
type scriptWriter struct {
	mu      sync.Mutex
	steps   []writeStep
	calls   int
	written [][]byte
	defFull bool // when the script runs out, write fully (or fail if set)
	failErr error
}

type writeStep struct {
	n    int
	err  error
	full bool
}

func (w *scriptWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	cp := append([]byte(nil), p...)
	if w.calls <= len(w.steps) {
		st := w.steps[w.calls-1]
		if st.full {
			w.written = append(w.written, cp)
			return len(p), nil
		}
		w.written = append(w.written, cp[:st.n])
		return st.n, st.err
	}
	if w.failErr != nil {
		return 0, w.failErr
	}
	w.written = append(w.written, cp)
	return len(p), nil
}

func (w *scriptWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *scriptWriter) totalWritten() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, c := range w.written {
		n += len(c)
	}
	return n
}

func (w *scriptWriter) numCalls() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// submitSync submits chunk and reports the reader-side result.
func submitSync(s *Scheduler, f *Flow, chunk []byte) error {
	done := make(chan error, 1)
	go func() { done <- s.Submit(f, chunk) }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("submit hung")
	}
}

// TestShortWriteRequeuesRemainder: a writer that takes half a chunk with no
// error must not lose the tail: the scheduler requeues it, the reader stays
// parked, and the full chunk eventually lands.
func TestShortWriteRequeuesRemainder(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	w := &scriptWriter{steps: []writeStep{{n: 512}, {full: true}}}
	f := s.AddFlow(w)
	chunk := bytes.Repeat([]byte{7}, 1024)
	if err := submitSync(s, f, chunk); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The remainder is served on a later turn; poll for completion.
	deadline := time.Now().Add(5 * time.Second)
	for w.totalWritten() < len(chunk) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := w.totalWritten(); got != len(chunk) {
		t.Fatalf("writer got %d bytes, want full chunk %d (tail dropped?)", got, len(chunk))
	}
	if c := w.numCalls(); c < 2 {
		t.Fatalf("writer calls %d, want >= 2 (half + remainder)", c)
	}
}

// TestZeroProgressFailsFast: a writer returning (0, nil) forever makes no
// progress by definition; requeueing it would spin the single writer loop
// and starve every flow. The flow must fail fast instead, with a bounded
// number of writer calls (no spin).
func TestZeroProgressFailsFast(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	w := &scriptWriter{steps: []writeStep{{n: 0}}}
	f := s.AddFlow(w)
	err := submitSync(s, f, bytes.Repeat([]byte{9}, 64))
	if !errors.Is(err, ErrFlowClosed) {
		t.Fatalf("submit err %v, want ErrFlowClosed (dead writer fails fast)", err)
	}
	if c := w.numCalls(); c > 2 {
		t.Fatalf("writer calls %d, want <= 2 (spin detected)", c)
	}
	// The scheduler itself must still serve other flows afterwards.
	w2 := &scriptWriter{}
	f2 := s.AddFlow(w2)
	if err := submitSync(s, f2, []byte("alive")); err != nil {
		t.Fatalf("scheduler wedged after dead flow: %v", err)
	}
}

// TestFatalWriteErrorDropsFlow: a non-timeout error kills exactly that flow;
// the reader observes ErrFlowClosed and the scheduler keeps serving.
func TestFatalWriteErrorDropsFlow(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	boom := errors.New("boom")
	w := &scriptWriter{failErr: boom}
	f := s.AddFlow(w)
	if err := submitSync(s, f, []byte("doomed")); !errors.Is(err, ErrFlowClosed) {
		t.Fatalf("submit err %v, want ErrFlowClosed", err)
	}
	w2 := &scriptWriter{}
	if err := submitSync(s, s.AddFlow(w2), []byte("alive")); err != nil {
		t.Fatalf("scheduler wedged after fatal flow: %v", err)
	}
	if got := w2.totalWritten(); got != 5 {
		t.Fatalf("healthy flow wrote %d bytes, want 5", got)
	}
}

// TestSubmitTimeoutCountsOnce: a parked submit that times out detaches
// exactly once (no double timedOut, no queue residue) even if the writer
// later picks the detached flow.
func TestSubmitTimeoutCountsOnce(t *testing.T) {
	cfg := testCfg()
	cfg.SubmitTimeout = 50 * time.Millisecond
	s := NewScheduler(cfg)
	// No Run: the submit parks, then must fail fast with ErrBusy.
	f := s.AddFlow(&scriptWriter{})
	start := time.Now()
	err := s.Submit(f, []byte("parked"))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("submit err %v, want ErrBusy", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("fail-fast took %v", d)
	}
	st := s.Stats()
	if st.TimedOut != 1 {
		t.Fatalf("timedOut=%d, want exactly 1", st.TimedOut)
	}
	if q := s.QueuedLen(); q != 0 {
		t.Fatalf("queued=%d after timeout, want 0 (detached)", q)
	}
}

// TestPumpZeroReadNoSpin: a reader returning (0, nil) repeatedly must not
// wedge Pump or lose the subsequent data.
func TestPumpZeroReadNoSpin(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	r := &flakyReader{zeros: 1000, data: []byte("hello")}
	w := &scriptWriter{}
	f := s.AddFlow(w)
	done := make(chan error, 1)
	go func() { done <- s.Pump(f, r) }()
	select {
	case err := <-done:
		if !errors.Is(err, errTestEOF) {
			t.Fatalf("pump err %v, want test EOF after data", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pump hung on (0, nil) reads")
	}
	if got := w.totalWritten(); got != 5 {
		t.Fatalf("wrote %d bytes, want 5", got)
	}
}

type flakyReader struct {
	zeros int
	data  []byte
	off   int
}

func (r *flakyReader) Read(p []byte) (int, error) {
	if r.zeros > 0 {
		r.zeros--
		return 0, nil
	}
	if r.off >= len(r.data) {
		return 0, errors.New("EOF-sentinel")
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	if r.off >= len(r.data) {
		return n, errTestEOF
	}
	return n, nil
}

var errTestEOF = errors.New("test EOF")

// TestConcurrentSubmitSeparateFlows is a race-detector workout: many flows
// submitting concurrently must all be served exactly once, with byte-exact
// totals. (One Flow shared across goroutines is caller misuse — a Flow is
// per stream — so the supported pattern tested here is one Flow each.)
func TestConcurrentSubmitSeparateFlows(t *testing.T) {
	s := NewScheduler(testCfg())
	go s.Run()
	defer s.Close()
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := &scriptWriter{}
			f := s.AddFlow(w)
			chunk := bytes.Repeat([]byte{byte(i)}, 100)
			if err := s.Submit(f, chunk); err != nil {
				errs <- err
				return
			}
			// The chunk must land whole: allow the writer loop a turn.
			deadline := time.Now().Add(5 * time.Second)
			for w.totalWritten() < len(chunk) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := w.totalWritten(); got != len(chunk) {
				errs <- errors.New("short flow delivery")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestLingerZeroLastServe: Linger on a flow that never served returns
// immediately (no sleep on the zero timestamp).
func TestLingerZeroLastServe(t *testing.T) {
	s := NewScheduler(testCfg())
	f := s.AddFlow(&scriptWriter{})
	start := time.Now()
	s.Linger(f)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("linger on unserved flow took %v", d)
	}
}
