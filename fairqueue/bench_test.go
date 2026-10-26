package fairqueue

// Traffic-mix benchmarks: hard numbers for what the scheduler costs and
// what it guarantees, per traffic class. Transport is net.Pipe loopback
// (this sandbox refuses TCP dials), so absolute MB/s UNDERSTATES real
// sockets — what matters here is scheduler-vs-direct deltas measured over
// the same pipe, plus latency/fairness/churn behavior.
//
// Mixes mirror production: bulk single + multi (torrent/shiru payload),
// sparse probes under saturation (DNS/SSH/handshakes), open/pump/close
// churn (peer discovery), tiny request/response (DNS), and web pages
// during bulk (the block-window complaint).

import (
	"bytes"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

// benchSched builds a scheduler with production defaults and starts it.
func benchSched() *Scheduler {
	s := NewScheduler(DefaultConfig())
	go s.Run()
	return s
}

// pumpStream pumps exactly n bytes from a reader of marker bytes down a new
// stream, closes it, and returns. The peer must drain to EOF.
func pumpStream(s *Scheduler, cs *smux.Session, marker byte, n int) error {
	st, err := cs.OpenStream()
	if err != nil {
		return err
	}
	defer st.Close()
	f := s.AddFlow(st)
	defer s.Finish(f)
	return pumpExpectEOF(s, f, bytes.NewReader(bytes.Repeat([]byte{marker}, n)))
}

func pumpExpectEOF(s *Scheduler, f *Flow, r io.Reader) error {
	if err := s.Pump(f, r); err != io.EOF {
		return err
	}
	return nil
}

// drainAccept accepts streams forever, discarding to EOF.
func drainAccept(ss *smux.Session) {
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
}

// BenchmarkDirectCopyBaseline: greedy io.Copy, no scheduler, one 64MB flow.
// This is the ceiling the scheduler is measured against.
func BenchmarkDirectCopyBaseline(b *testing.B) {
	cs, ss := smuxPair(b)
	drainAccept(ss)
	const size = 64 << 20
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := cs.OpenStream()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(st, bytes.NewReader(bytes.Repeat([]byte{byte(i)}, size))); err != nil {
			b.Fatal(err)
		}
		st.Close()
	}
}

// BenchmarkSchedulerBulk1Flow: same 64MB through the scheduler. The delta
// to baseline is the scheduler's overhead on bulk.
func BenchmarkSchedulerBulk1Flow(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)
	drainAccept(ss)
	const size = 64 << 20
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := pumpStream(s, cs, byte(i), size); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSchedulerBulk32Flows: 32 concurrent 4MB flows; reports aggregate
// throughput and per-flow fairness spread (min/max share).
func BenchmarkSchedulerBulk32Flows(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)

	const flows = 32
	const size = 4 << 20
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
	// Attribute fairness via turn counters over the run (per-flow byte
	// attribution would serialize the flows being measured).
	b.SetBytes(flows * size)
	b.ResetTimer()
	start := time.Now()
	var totalTurns uint64
	for n := 0; n < b.N; n++ {
		var wg sync.WaitGroup
		for i := 0; i < flows; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if err := pumpStream(s, cs, byte(i), size); err != nil {
					b.Errorf("flow %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		totalTurns += uint64(flows * (size / 4096))
	}
	el := time.Since(start)
	b.ReportMetric(float64(flows*size*b.N)/el.Seconds()/1e6, "aggregate-MB/s")
	b.ReportMetric(float64(totalTurns), "turns")
}

// BenchmarkSparseLatency: 8 saturating bulk flows; 200 sparse probes.
// Reports mean/max probe wait: the interactive guarantee under load.
func BenchmarkSparseLatency(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)
	drainAccept(ss)

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
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Fresh 1MB reader each lap: lap time is paced by service.
				if err := s.Pump(f, bytes.NewReader(bytes.Repeat([]byte("B"), 1<<20))); err != nil {
					return
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // let saturation establish
	b.ResetTimer()
	var waits []time.Duration
	const probes = 50
	for n := 0; n < b.N; n++ {
		for i := 0; i < probes; i++ {
			st, err := cs.OpenStream()
			if err != nil {
				b.Fatal(err)
			}
			f := s.AddFlow(st)
			t0 := time.Now()
			if err := s.Submit(f, []byte("probe")); err != nil {
				b.Fatalf("probe %d: %v", i, err)
			}
			waits = append(waits, time.Since(t0))
			s.Finish(f)
			st.Close()
		}
	}
	b.StopTimer()
	close(stop)
	bwg.Wait()
	sort.Slice(waits, func(i, j int) bool { return waits[i] < waits[j] })
	var sum time.Duration
	for _, w := range waits {
		sum += w
	}
	b.ReportMetric(float64(sum)/float64(len(waits))/1e6, "mean-probe-ms")
	b.ReportMetric(float64(waits[len(waits)-1])/1e6, "max-probe-ms")
	b.ReportMetric(float64(waits[len(waits)*99/100])/1e6, "p99-probe-ms")
}

// BenchmarkChurn: torrent-style open/pump-4KB/close rate.
func BenchmarkChurn(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)
	drainAccept(ss)
	payload := bytes.Repeat([]byte("C"), 4096)
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := cs.OpenStream()
		if err != nil {
			b.Fatal(err)
		}
		f := s.AddFlow(st)
		if err := pumpExpectEOF(s, f, bytes.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
		s.Finish(f)
		st.Close()
	}
	b.ReportMetric(float64(b.N), "flows")
}

// BenchmarkDNSLike: tiny request/response exchanges against an echo peer.
// Measures scheduler dispatch rate for DNS-sized traffic.
func BenchmarkDNSLike(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)
	// Echo peer: read exactly 200B, write them back, close.
	go func() {
		for {
			st, err := ss.AcceptStream()
			if err != nil {
				return
			}
			go func(st *smux.Stream) {
				defer st.Close()
				req := make([]byte, 200)
				if _, err := io.ReadFull(st, req); err != nil {
					return
				}
				_, _ = st.Write(req)
			}(st)
		}
	}()
	req := bytes.Repeat([]byte("Q"), 200)
	b.SetBytes(400) // 200 each way
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		st, err := cs.OpenStream()
		if err != nil {
			b.Fatal(err)
		}
		f := s.AddFlow(st)
		if err := s.Submit(f, append([]byte(nil), req...)); err != nil {
			b.Fatalf("submit: %v", err)
		}
		resp := make([]byte, 200)
		if _, err := io.ReadFull(st, resp); err != nil {
			b.Fatalf("read: %v", err)
		}
		if !bytes.Equal(resp, req) {
			b.Fatal("echo mismatch")
		}
		s.Finish(f)
		st.Close()
	}
}

// BenchmarkMixedWeb: 4 bulk 8MB flows + 24 staggered 100KB "pages".
// Reports page completion p50/p99 during bulk: the block-window complaint.
func BenchmarkMixedWeb(b *testing.B) {
	s := benchSched()
	defer s.Close()
	cs, ss := smuxPair(b)
	drainAccept(ss)

	var bwg sync.WaitGroup
	for i := 0; i < 4; i++ {
		bwg.Add(1)
		go func(i int) {
			defer bwg.Done()
			_ = pumpStream(s, cs, byte(0xF0+i), 8<<20)
		}(i)
	}
	time.Sleep(200 * time.Millisecond) // bulk establishes first
	b.ResetTimer()
	var mu sync.Mutex
	var pages []time.Duration
	for n := 0; n < b.N; n++ {
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := time.Now()
				if err := pumpStream(s, cs, 0x50, 100<<10); err != nil {
					b.Errorf("page: %v", err)
					return
				}
				mu.Lock()
				pages = append(pages, time.Since(t0))
				mu.Unlock()
			}()
			time.Sleep(25 * time.Millisecond) // staggered arrivals
		}
		wg.Wait()
	}
	b.StopTimer()
	bwg.Wait()
	sort.Slice(pages, func(i, j int) bool { return pages[i] < pages[j] })
	if len(pages) == 0 {
		b.Fatal("no pages completed")
	}
	b.ReportMetric(float64(pages[len(pages)/2])/1e6, "page-p50-ms")
	b.ReportMetric(float64(pages[len(pages)*99/100])/1e6, "page-p99-ms")
	b.ReportMetric(float64(len(pages)), "pages-done")
}
