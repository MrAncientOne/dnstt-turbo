// dnstt-client is the client end of a DNS tunnel.
//
// Usage:
//
//	dnstt-client [-doh URL|-dot ADDR|-udp ADDR] -pubkey-file PUBKEYFILE DOMAIN LOCALADDR
//
// Examples:
//
//	dnstt-client -doh https://resolver.example/dns-query -pubkey-file server.pub t.example.com 127.0.0.1:7000
//	dnstt-client -dot resolver.example:853 -pubkey-file server.pub t.example.com 127.0.0.1:7000
//
// The program supports DNS over HTTPS (DoH), DNS over TLS (DoT), and UDP DNS.
// Use one of these options:
//
//	-doh https://resolver.example/dns-query
//	-dot resolver.example:853
//	-udp resolver.example:53
//
// You can give the server's public key as a file or as a hex string. Use
// "dnstt-server -gen-key" to get the public key.
//
//	-pubkey-file server.pub
//	-pubkey 0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff
//
// DOMAIN is the root of the DNS zone reserved for the tunnel. See README for
// instructions on setting it up.
//
// LOCALADDR is the TCP address that will listen for connections and forward
// them over the tunnel.
//
// In -doh and -dot modes, the program's TLS fingerprint is camouflaged with
// uTLS by default. The specific TLS fingerprint is selected randomly from a
// weighted distribution. You can set your own distribution (or specific single
// fingerprint) using the -utls option. The special value "none" disables uTLS.
//
//	-utls '3*Firefox,2*Chrome,1*iOS'
//	-utls Firefox
//	-utls none
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/bbr"
	"www.bamsoftware.com/git/dnstt.git/diagnostics"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/fairqueue"
	"www.bamsoftware.com/git/dnstt.git/noise"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

// smux streams will be closed after this much time without receiving data.
var health = diagnostics.New("client")

const idleTimeout = 2 * time.Minute

var errSessionLost = errors.New("tunnel session lost")

// validateUpstreamMTU bounds -tcp-upstream-mtu. The lower bound is KCP's
// minimum segment scale; the upper bound is kcp-go's session MTU cap
// (mtuLimit 1500): above it SetMtu refuses, and near it a query's EDNS
// option length would no longer fit its 16-bit field alongside framing.
// Pure function so the CLI contract is unit-tested.
func validateUpstreamMTU(mtu int) error {
	if mtu < 80 {
		return fmt.Errorf("invalid -tcp-upstream-mtu %d: minimum 80", mtu)
	}
	if mtu > 1500 {
		return fmt.Errorf("invalid -tcp-upstream-mtu %d: maximum 1500 (kcp-go session cap)", mtu)
	}
	return nil
}

// dnsNameCapacity returns the number of bytes remaining for encoded data after
// including domain in a DNS name.
func dnsNameCapacity(domain dns.Name) int {
	// Names must be 255 octets or shorter in total length.
	// https://tools.ietf.org/html/rfc1035#section-2.3.4
	capacity := 255
	// Subtract the length of the null terminator.
	capacity -= 1
	for _, label := range domain {
		// Subtract the length of the label and the length octet.
		capacity -= len(label) + 1
	}
	// Each label may be up to 63 bytes long and requires 64 bytes to
	// encode.
	capacity = capacity * 63 / 64
	// Base32 expands every 5 bytes to 8.
	capacity = capacity * 5 / 8
	return capacity
}

// readKeyFromFile reads a key from a named file.
func readKeyFromFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return noise.ReadKey(f)
}

// sampleUTLSDistribution parses a weighted uTLS Client Hello ID distribution
// string of the form "3*Firefox,2*Chrome,1*iOS", matches each label to a
// utls.ClientHelloID from utlsClientHelloIDMap, and randomly samples one
// utls.ClientHelloID from the distribution.
func sampleUTLSDistribution(spec string) (*utls.ClientHelloID, error) {
	weights, labels, err := parseWeightedList(spec)
	if err != nil {
		return nil, err
	}
	var total uint64
	for _, w := range weights {
		total += uint64(w)
	}
	if total == 0 {
		// sampleWeighted panics on a zero total; a "-utls 0*Firefox"
		// typo must be a startup error, never a crash.
		return nil, fmt.Errorf("total weight is zero in %q", spec)
	}
	ids := make([]*utls.ClientHelloID, 0, len(labels))
	for _, label := range labels {
		var id *utls.ClientHelloID
		if label == "none" {
			id = nil
		} else {
			id = utlsLookup(label)
			if id == nil {
				return nil, fmt.Errorf("unknown TLS fingerprint %q", label)
			}
		}
		ids = append(ids, id)
	}
	return ids[sampleWeighted(weights)], nil
}

type halfCloseConn interface {
	net.Conn
	CloseRead() error
	CloseWrite() error
}

func handle(local halfCloseConn, sess *smux.Session, conv uint32, sched *fairqueue.Scheduler) error {
	stream, err := sess.OpenStream()
	if err != nil {
		// smux records terminal I/O errors without closing CloseChan. Wake
		// admission, handlers and the outer retry loop instead of retaining
		// a session which can never open another stream.
		sess.Close()
		return fmt.Errorf("session %08x opening stream: %v", conv, err)
	}
	defer func() {
		log.Printf("end stream %08x:%d", conv, stream.ID())
		stream.Close()
	}()
	log.Printf("begin stream %08x:%d", conv, stream.ID())
	health.ActiveStreams.Add(1)
	defer health.ActiveStreams.Add(-1)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Upload (stream <- local). When the fair scheduler is enabled it paces
		// this flow into smux in sparse-priority round-robin order with every
		// other flow on the session; otherwise copy greedily as upstream.
		var err error
		if sched != nil {
			f := sched.AddFlow(stream)
			err = sched.Pump(f, local)
			if err == io.EOF {
				// Clean end: let queued tail frames flush before FIN,
				// which would otherwise jump them in smux's send queue.
				sched.Linger(f)
			}
			sched.Finish(f)
		} else {
			_, err = io.Copy(stream, local)
		}
		if err == io.EOF {
			// smux Stream.Write may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, fairqueue.ErrFlowClosed) && !errors.Is(err, fairqueue.ErrBusy) {
			log.Printf("stream %08x:%d copy stream←local: %v", conv, stream.ID(), err)
		}
		local.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, stream)
		if err == io.EOF {
			// smux Stream.WriteTo may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("stream %08x:%d copy local←stream: %v", conv, stream.ID(), err)
		}
		local.CloseWrite()
	}()
	wg.Wait()

	return err
}

// Tuning knobs owned by main/run, defaulted to the proven values and set from
// flags in main(). kcpWindow is the KCP send/recv window in segments (the main
// throughput vs. bufferbloat lever); streamBuffer is smux's per-stream buffer.
const turboBuild = "2026-10-24-auditfix2"

var (
	kcpWindow    = 128
	streamBuffer = 1 * 1024 * 1024
	// maxStreams bounds the number of concurrent tunneled streams. 0 means
	// unlimited (upstream behavior). A positive value backpressures the local
	// accept loop so a runaway client (e.g. BitTorrent opening thousands of
	// peer connections) cannot flood the single shared session and starve
	// interactive/DNS streams.
	maxStreams = 0

	// Fair upload scheduler knobs (see -fair-upload and fairqueue.Config).
	fairUpload        = false
	fairChunk         = 4096
	fairIdle          = 100 * time.Millisecond
	fairWriteTimeout  = 500 * time.Millisecond
	fairMaxBurst      = 8
	fairMaxSparse     = 64
	fairSubmitTimeout = time.Duration(0)
	fairLinger        = 250 * time.Millisecond

	// Throughput tunables. All default to current behavior (off/unchanged).
	// See -tcp-sockbuf, -smux-recvbuf, -kcp-acknodelay.
	tcpSockBuf    = 0
	smuxRecvBuf   = 4194304 // smux default session-wide receive buffer
	kcpACKNoDelay = false
	// BBR-lite adaptive congestion control (estimator+pacer, client side).
	// Off = today's static window, byte for byte.
	bbrOn = false
)

func run(pubkey []byte, domain dns.Name, localAddr *net.TCPAddr, remoteAddr net.Addr, pconn net.PacketConn, mtu int) error {
	defer pconn.Close()
	stopHealth := health.Start()
	defer stopHealth()
	log.Printf("role=client event=session_config transport=%T mtu=%d kcp_window=%d bbr=%t fair_upload=%t stream_buffer=%d smux_recvbuf=%d", pconn, mtu, kcpWindow, bbrOn, fairUpload, streamBuffer, smuxRecvBuf)

	ln, err := net.ListenTCP("tcp", localAddr)
	if err != nil {
		return fmt.Errorf("opening local listener: %v", err)
	}
	defer ln.Close()
	log.Printf("role=client event=local_listening address=%q", ln.Addr().String())

	if mtu < 80 {
		return fmt.Errorf("upstream MTU is only %d bytes", mtu)
	}
	log.Printf("effective MTU %d", mtu)

	// Open a KCP conn on the PacketConn.
	conn, err := kcp.NewConn2(remoteAddr, nil, 0, 0, pconn)
	if err != nil {
		return fmt.Errorf("opening KCP conn: %v", err)
	}
	defer func() {
		log.Printf("end session %08x", conn.GetConv())
		conn.Close()
	}()
	log.Printf("begin session %08x", conn.GetConv())
	// Permit coalescing the payloads of consecutive sends.
	conn.SetStreamMode(true)
	// Disable the dynamic congestion window (limit only by the maximum of
	// local and remote static windows).
	conn.SetNoDelay(
		0, // default nodelay
		0, // default interval
		0, // default resend
		1, // nc=1 => congestion window off
	)
	if kcpACKNoDelay {
		// Flush ACKs immediately instead of batching at the update
		// interval: lower latency and faster window growth, at the cost
		// of more (small) upstream query traffic.
		conn.SetACKNoDelay(true)
	}
	conn.SetWindowSize(kcpWindow, kcpWindow)
	if !conn.SetMtu(mtu) {
		conn.Close()
		return fmt.Errorf("KCP refused upstream MTU %d (kcp-go caps at 1500; use -tcp-upstream-mtu 0..1500)", mtu)
	}
	log.Printf("KCP window %d segments, upstream MTU %d", kcpWindow, mtu)

	// Bound handshakes as well as established-session liveness. A peer may
	// have restarted and forgotten the old KCP/Noise state.
	handshakeTimer := time.AfterFunc(30*time.Second, func() { conn.Close() })
	log.Printf("role=client event=handshake_started session=%08x", conn.GetConv())
	started := time.Now()
	rw, err := noise.NewClient(conn, pubkey)
	handshakeTimer.Stop()
	if err != nil {
		health.Failure("handshake_failed", err)
		return fmt.Errorf("%w: handshake: %v", errSessionLost, err)
	}

	// Start a smux session on the Noise channel.
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	smuxConfig.MaxStreamBuffer = streamBuffer // upstream dnstt default is 65536
	if smuxRecvBuf < streamBuffer {
		log.Printf("smux-recvbuf %d < stream-buffer %d, clamping up", smuxRecvBuf, streamBuffer)
		smuxRecvBuf = streamBuffer
	}
	smuxConfig.MaxReceiveBuffer = smuxRecvBuf // session-wide; must stay >= streamBuffer
	sess, err := smux.Client(rw, smuxConfig)
	if err != nil {
		return fmt.Errorf("opening smux session: %v", err)
	}
	defer sess.Close()
	// smux's error notifications do not themselves close CloseChan. A Noise
	// record failure is terminal even if carrier polling continues to work.
	if terminal, ok := rw.(interface{ Done() <-chan struct{} }); ok {
		closeSessionOn(sess, terminal.Done())
	}
	health.ActiveSessions.Add(1)
	defer health.ActiveSessions.Add(-1)
	log.Printf("role=client event=session_ready session=%08x handshake_ms=%d", conn.GetConv(), time.Since(started).Milliseconds())
	if dc, ok := pconn.(*DNSPacketConn); ok {
		closeSessionOn(sess, dc.Done())
	}

	// BBR-lite (-bbr): one estimator+pacer per session, client side only.
	// Download in-flight is governed unilaterally by the receive window we
	// advertise (KCP honors min of both ends), and everything upstream
	// rides our queries, which the pacer shapes. The server needs no code
	// and no flag: give it a high static ceiling (e.g. -kcp-window 1024)
	// that our advertised window stays under. Nil controller = today's
	// static behavior, byte for byte.
	//
	// One fair upload scheduler for the session (nil = greedy, upstream
	// behavior). Created before BBR so the estimator ticker can drive its
	// CAKE admission control live on slow paths. At high measured rates,
	// payload pacing uses only the carrier clock, retaining flow fairness.
	var sched *fairqueue.Scheduler
	if fairUpload {
		sched = fairqueue.NewScheduler(fairqueue.Config{
			Chunk:         fairChunk,
			Idle:          fairIdle,
			WriteTimeout:  fairWriteTimeout,
			MaxBurst:      fairMaxBurst,
			MaxSparse:     fairMaxSparse,
			SubmitTimeout: fairSubmitTimeout,
			Reliable:      fairSubmitTimeout <= 0,
			Linger:        fairLinger,
		})
		go sched.Run()
		defer sched.Close()
		// Periodic counters: timedOut climbing during bulk means the
		// scheduler is starving flows (fail-fast firing); bulkTurns
		// stalled with queued>0 means the writer is wedged. Read these
		// before any other diagnosis.
		stopStats := make(chan struct{})
		defer close(stopStats)
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stopStats:
					return
				case <-t.C:
					st := sched.Stats()
					log.Printf("fair upload stats: sparse=%d bulk=%d demoted=%d timedout=%d dropped=%d queued=%d",
						st.SparseTurns, st.BulkTurns, st.Demoted, st.TimedOut, st.Dropped, sched.QueuedLen())
				}
			}
		}()
		log.Printf("fair upload scheduling on (CAKE: DRR++ deficit, Cobalt AQM, adaptive admission shaping)")
	}
	if bbrOn {
		if dc, ok := pconn.(*DNSPacketConn); ok {
			control := newAdaptiveControl(mtu, nil)
			prof := &bbr.KCPProfiler{}
			downProf := &bbr.KCPProfiler{}
			dc.feedback.SetFlight(control.up.SendWindowSegments(), mtu-bbr.KCPHeaderSize)
			dc.pacer.SetRate(control.up.PacingRate(), float64(mtu))
			ratesPath := os.Getenv("BBR_RATES_FILE")
			if ratesPath == "" {
				ratesPath = defaultRatesPath
			}
			bbrDone, bbrStopped := make(chan struct{}), make(chan struct{})
			defer func() { close(bbrDone); <-bbrStopped }()
			go func() {
				defer close(bbrStopped)
				t := time.NewTicker(100 * time.Millisecond)
				defer t.Stop()
				for n := 0; ; n++ {
					select {
					case <-bbrDone:
						return
					case <-t.C:
						up, down := dc.feedback.Tick()
						dnsSample := dc.mon.tick()
						if !up.HasRTT {
							up.RTT, up.HasRTT = dnsSample.RTT, dnsSample.HasRTT
						}
						down.RTT, down.HasRTT = dnsSample.RTT, dnsSample.HasRTT
						d := control.update(up, down, dc.feedback.DownstreamSegmentBytes(), dc.feedback.AvgResponse())
						conn.SetWindowSize(d.sendWindow, d.receiveWindow)
						dc.feedback.SetFlight(d.sendWindow, mtu-bbr.KCPHeaderSize)
						dc.pacer.SetRate(d.pacingRate, d.burst)
						SetPollTarget(d.polls)
						if sched != nil {
							sched.SetRate(uint64(d.shaperRate), control.up.MinRTT())
						}
						if changed, degraded := prof.Update(control.up.SustainedBW()); changed {
							if degraded {
								conn.SetNoDelay(0, 10, 2, 1)
								conn.SetACKNoDelay(true)
								control.up.ExitStartupEarly()
							} else {
								conn.SetNoDelay(0, 10, 0, 1)
								conn.SetACKNoDelay(kcpACKNoDelay)
							}
						}
						downProf.Update(control.down.SustainedBW())
						if n%300 == 0 {
							log.Printf("role=client event=adaptive send_window=%d receive_window=%d polls=%d pacing_Bps=%.0f shaper_Bps=%.0f", d.sendWindow, d.receiveWindow, d.polls, d.pacingRate, float64(d.shaperRate))
						}
						if n%10 == 0 {
							// Preserve downstream fields for existing consumers; expose upload separately.
							body := formatRates(time.Now(), control.down.MaxBW(), control.down.SustainedBW(), control.down.MinRTT(), control.down.State(), downProf.Degraded(), downProf.Best())
							body += fmt.Sprintf("UP_BW_BPS=%.0f\nUP_SUSTAINED_BPS=%.0f\n", control.up.MaxBW(), control.up.SustainedBW())
							if err := publishRates(ratesPath, body); err != nil && n%300 == 0 {
								log.Printf("bbr rates publish: %v", err)
							}
						}
					}
				}
			}()
			log.Printf("bbr-lite: independent upload/download models, windows %d..%d, flight budget %d bytes/direction", bbrMinWindow, bbrMaxWindow, bbrMaxFlight)
		}
	}

	return serveLocalStreams(ln, sess, conn.GetConv(), sched)
}

// The local listener and every active local socket must wake when the session
// dies, including when admission control is full or an app is idle in Read.
func closeSessionOn(sess *smux.Session, done <-chan struct{}) {
	go func() {
		select {
		case <-done:
			sess.Close()
		case <-sess.CloseChan():
		}
	}()
}

func serveLocalStreams(ln net.Listener, sess *smux.Session, conv uint32, sched *fairqueue.Scheduler) error {
	var workers sync.WaitGroup
	defer func() { sess.Close(); workers.Wait() }()
	go func() { <-sess.CloseChan(); ln.Close() }()
	// Admission control: when maxStreams > 0, hold at most that many streams
	// open at once. Acquiring a slot *before* Accept means that at capacity we
	// stop accepting, so pending local connections queue in the kernel backlog
	// (backpressure to tun2socks/the app) instead of piling onto the session.
	var sem chan struct{}
	if maxStreams > 0 {
		sem = make(chan struct{}, maxStreams)
		log.Printf("stream admission cap: %d concurrent", maxStreams)
	}

	for {
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-sess.CloseChan():
				return errSessionLost
			}
		}
		local, err := ln.Accept()
		if err != nil {
			if sem != nil {
				<-sem
			}
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			if sess.IsClosed() {
				return errSessionLost
			}
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			finished := make(chan struct{})
			defer close(finished)
			go func() {
				select {
				case <-sess.CloseChan():
					local.Close()
				case <-finished:
				}
			}()
			defer local.Close()
			if sem != nil {
				defer func() { <-sem }()
			}
			err := handle(local.(halfCloseConn), sess, conv, sched)
			if err != nil {
				log.Printf("handle: %v", err)
			}
		}()
	}
}

func main() {
	var dohURL string
	var dotAddr string
	var tcpAddr string
	var tcpUpstreamMTU int
	var pubkeyFilename string
	var pubkeyString string
	var udpAddr string
	var utlsDistribution string
	// Performance tuning (defaults = proven values; override for a faster or
	// more conservative link). See -h and TURBO_README.md.
	var fKCPWindow, fPollLimit, fPollBurst, fStreamBuffer, fMaxStreams, fFairChunk, fFairMaxBurst, fFairMaxSparse, fTCPSockBuf, fSmuxRecvBuf int
	var fInitPollDelay, fMaxPollDelay, fFairIdle, fFairWriteTimeout, fFairSubmitTimeout, fFairLinger time.Duration
	var fKCPACKNoDelay bool
	var fBBR bool
	var fFairUpload bool

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s [-doh URL|-dot ADDR|-udp ADDR] -pubkey-file PUBKEYFILE DOMAIN LOCALADDR

Examples:
  %[1]s -doh https://resolver.example/dns-query -pubkey-file server.pub t.example.com 127.0.0.1:7000
  %[1]s -dot resolver.example:853 -pubkey-file server.pub t.example.com 127.0.0.1:7000

`, os.Args[0])
		flag.PrintDefaults()
		labels := make([]string, 0, len(utlsClientHelloIDMap))
		labels = append(labels, "none")
		for _, entry := range utlsClientHelloIDMap {
			labels = append(labels, entry.Label)
		}
		fmt.Fprintf(flag.CommandLine.Output(), `
Known TLS fingerprints for -utls are:
`)
		i := 0
		for i < len(labels) {
			var line strings.Builder
			fmt.Fprintf(&line, "  %s", labels[i])
			w := 2 + len(labels[i])
			i++
			for i < len(labels) && w+1+len(labels[i]) <= 72 {
				fmt.Fprintf(&line, " %s", labels[i])
				w += 1 + len(labels[i])
				i++
			}
			fmt.Fprintln(flag.CommandLine.Output(), line.String())
		}
	}
	flag.StringVar(&dohURL, "doh", "", "URL of DoH resolver")
	flag.StringVar(&dotAddr, "dot", "", "address of DoT resolver")
	flag.StringVar(&pubkeyString, "pubkey", "", fmt.Sprintf("server public key (%d hex digits)", noise.KeyLen*2))
	flag.StringVar(&pubkeyFilename, "pubkey-file", "", "read server public key from file")
	flag.StringVar(&udpAddr, "udp", "", "address of UDP DNS resolver")
	flag.StringVar(&tcpAddr, "tcp", "", "address of plain DNS-over-TCP server (direct to the tunnel server on :53)")
	flag.IntVar(&tcpUpstreamMTU, "tcp-upstream-mtu", 0, "with -tcp, carry upstream data in an EDNS option of this many bytes/query (0 = robust QNAME-bound ~132 B; e.g. 1400 for max upload if the path allows it)")
	flag.StringVar(&utlsDistribution, "utls",
		"4*random,3*Firefox_120,1*Firefox_105,3*Chrome_120,1*Chrome_102,1*iOS_14,1*iOS_13",
		"choose TLS fingerprint from weighted distribution")
	// --- performance tuning flags ---
	flag.IntVar(&fKCPWindow, "kcp-window", kcpWindow, "KCP send/recv window in segments (throughput vs. bufferbloat; ~2-3x BDP/MTU)")
	flag.IntVar(&fPollLimit, "poll-limit", pollLimit, "baseline in-flight polls (with -bbr grows automatically to fit measured BDP)")
	flag.IntVar(&fPollBurst, "poll-burst", pollBurst, "polls enqueued per received data response (>=2 grows the download window)")
	flag.DurationVar(&fInitPollDelay, "init-poll-delay", initPollDelay, "initial idle poll interval")
	flag.DurationVar(&fMaxPollDelay, "max-poll-delay", maxPollDelay, "max idle poll interval (backoff ceiling)")
	flag.IntVar(&fStreamBuffer, "stream-buffer", streamBuffer, "smux per-stream buffer bytes")
	flag.IntVar(&fMaxStreams, "max-streams", maxStreams, "max concurrent tunneled streams, 0 = unlimited (caps a runaway client e.g. BitTorrent from flooding the single session)")
	// Fair upload scheduler: paces upload flows into smux in sparse-priority
	// round-robin order so a bulk uploader can't starve interactive uploads
	// (DNS/handshakes/requests). Default off = greedy. Self-tuning (no rate knob).
	flag.BoolVar(&fFairUpload, "fair-upload", fairUpload, "fair-queue the upload path (sparse-priority round-robin) so bulk can't starve interactive")
	flag.IntVar(&fFairChunk, "fair-chunk", fairChunk, "with -fair-upload: max bytes served per flow per turn")
	flag.DurationVar(&fFairIdle, "fair-idle", fairIdle, "with -fair-upload: idle gap that re-marks a flow sparse (interactive)")
	flag.DurationVar(&fFairWriteTimeout, "fair-write-timeout", fairWriteTimeout, "with -fair-upload: per-chunk smux write deadline (yields a stalled flow)")
	flag.IntVar(&fFairMaxBurst, "fair-max-burst", fairMaxBurst, "with -fair-upload: consecutive sparse turns before one bulk turn is forced (bulk starvation protection; <=0 = strict sparse)")
	flag.IntVar(&fFairMaxSparse, "fair-max-sparse", fairMaxSparse, "with -fair-upload: cap on the sparse list; overflow demotes to round-robin")
	flag.DurationVar(&fFairSubmitTimeout, "fair-submit-timeout", fairSubmitTimeout, "with -fair-upload: scheduler starvation timeout (0 = wait through congestion; positive expiry fails the stream)")
	flag.DurationVar(&fFairLinger, "fair-linger", fairLinger, "with -fair-upload: after clean pump end, wait for quiet before FIN so it can't jump queued tail data (0 = off)")
	// --- throughput tunables (all default to current behavior) ---
	flag.IntVar(&fTCPSockBuf, "tcp-sockbuf", tcpSockBuf, "with -tcp: kernel socket buffers (SO_RCVBUF/SO_SNDBUF) on the port-53 connection, 0 = OS default (e.g. 4194304 to absorb bursts)")
	flag.IntVar(&fSmuxRecvBuf, "smux-recvbuf", smuxRecvBuf, "smux session-wide receive buffer bytes (must stay >= -stream-buffer; raise only if the session bucket binds first)")
	flag.BoolVar(&fKCPACKNoDelay, "kcp-acknodelay", kcpACKNoDelay, "flush KCP ACKs immediately instead of batching at the update interval (lower latency, more upstream query traffic)")
	flag.BoolVar(&fBBR, "bbr", bbrOn, "BBR-lite: independent directional delivery estimates, adaptive bounded windows and upload pacing (default off)")
	flag.IntVar(&bbrMinWindow, "bbr-min-window", bbrMinWindow, "adaptive minimum window in segments")
	flag.IntVar(&bbrMaxWindow, "bbr-max-window", bbrMaxWindow, "adaptive maximum window (wire limit 65535)")
	flag.IntVar(&bbrMaxFlight, "bbr-max-flight", bbrMaxFlight, "adaptive payload flight budget per direction in bytes")
	flag.Float64Var(&bbrMinRate, "bbr-min-rate", bbrMinRate, "minimum upload probing rate in payload bytes/second")
	configureLog := diagnostics.RegisterLogFlags()
	flag.Parse()
	if err := configureLog(); err != nil {
		log.Fatal(err)
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("dnstt-turbo client build %s", turboBuild)

	// Apply tuning knobs (must happen before any QueuePacketConn/KCP conn).
	kcpWindow = fKCPWindow
	pollLimit = fPollLimit
	pollBurst = fPollBurst
	initPollDelay = fInitPollDelay
	maxPollDelay = fMaxPollDelay
	streamBuffer = fStreamBuffer
	maxStreams = fMaxStreams
	fairUpload = fFairUpload
	fairChunk = fFairChunk
	fairIdle = fFairIdle
	fairWriteTimeout = fFairWriteTimeout
	fairMaxBurst = fFairMaxBurst
	fairMaxSparse = fFairMaxSparse
	fairSubmitTimeout = fFairSubmitTimeout
	fairLinger = fFairLinger
	tcpSockBuf = fTCPSockBuf
	smuxRecvBuf = fSmuxRecvBuf
	kcpACKNoDelay = fKCPACKNoDelay
	bbrOn = fBBR
	if err := validateAdaptiveSettings(); err != nil {
		log.Fatal(err)
	}
	if bbrOn {
		explicit := make(map[string]bool)
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		if !explicit["stream-buffer"] {
			streamBuffer = bbrMaxFlight
		}
		if !explicit["smux-recvbuf"] {
			smuxRecvBuf = 2 * bbrMaxFlight
		}
	}
	// The turbotunnel queue must comfortably hold the window (packets past the
	// queue are dropped); size it to 2x the window, floor at the default.
	// With BBR, size for the configured ceiling up front.
	if bbrOn {
		turbotunnel.SetQueueSize(2 * bbrMaxWindow)
	} else {
		turbotunnel.SetQueueSize(kcpWindow * 2)
	}

	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(1)
	}
	domain, err := dns.ParseName(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid domain %+q: %v\n", flag.Arg(0), err)
		os.Exit(1)
	}
	localAddr, err := net.ResolveTCPAddr("tcp", flag.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var pubkey []byte
	if pubkeyFilename != "" && pubkeyString != "" {
		fmt.Fprintf(os.Stderr, "only one of -pubkey and -pubkey-file may be used\n")
		os.Exit(1)
	} else if pubkeyFilename != "" {
		var err error
		pubkey, err = readKeyFromFile(pubkeyFilename)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot read pubkey from file: %v\n", err)
			os.Exit(1)
		}
	} else if pubkeyString != "" {
		var err error
		pubkey, err = noise.DecodeKey(pubkeyString)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pubkey format error: %v\n", err)
			os.Exit(1)
		}
	}
	if len(pubkey) == 0 {
		fmt.Fprintf(os.Stderr, "the -pubkey or -pubkey-file option is required\n")
		os.Exit(1)
	}

	utlsClientHelloID, err := sampleUTLSDistribution(utlsDistribution)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parsing -utls: %v\n", err)
		os.Exit(1)
	}
	if utlsClientHelloID != nil {
		log.Printf("uTLS fingerprint %s %s", utlsClientHelloID.Client, utlsClientHelloID.Version)
	}

	// Iterate over the remote resolver address options and select one and
	// only one.
	var reopen func() (net.Addr, net.PacketConn, error)
	for _, opt := range []struct {
		s string
		f func(string) (net.Addr, net.PacketConn, error)
	}{
		// -doh
		{dohURL, func(s string) (net.Addr, net.PacketConn, error) {
			addr := turbotunnel.DummyAddr{}
			var rt http.RoundTripper
			if utlsClientHelloID == nil {
				transport := http.DefaultTransport.(*http.Transport).Clone()
				// Disable DefaultTransport's default Proxy =
				// ProxyFromEnvironment setting, for conformity
				// with utlsRoundTripper and with DoT mode,
				// which do not take a proxy from the
				// environment.
				transport.Proxy = nil
				rt = transport
			} else {
				rt = NewUTLSRoundTripper(nil, utlsClientHelloID)
			}
			pconn, err := NewHTTPPacketConn(rt, dohURL, 32)
			return addr, pconn, err
		}},
		// -dot
		{dotAddr, func(s string) (net.Addr, net.PacketConn, error) {
			addr := turbotunnel.DummyAddr{}
			var dialTLSContext func(ctx context.Context, network, addr string) (net.Conn, error)
			if utlsClientHelloID == nil {
				dialTLSContext = (&tls.Dialer{}).DialContext
			} else {
				dialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return utlsDialContext(ctx, network, addr, nil, utlsClientHelloID)
				}
			}
			pconn, err := NewTLSPacketConn(dotAddr, dialTLSContext)
			return addr, pconn, err
		}},
		// -udp
		{udpAddr, func(s string) (net.Addr, net.PacketConn, error) {
			addr, err := net.ResolveUDPAddr("udp", s)
			if err != nil {
				return nil, nil, err
			}
			pconn, err := net.ListenUDP("udp", nil)
			return addr, pconn, err
		}},
		// -tcp (plain DNS-over-TCP, direct to the tunnel server on :53)
		{tcpAddr, func(s string) (net.Addr, net.PacketConn, error) {
			addr, err := net.ResolveTCPAddr("tcp", s)
			if err != nil {
				return nil, nil, err
			}
			pconn, err := NewTCPPacketConn(s)
			return addr, pconn, err
		}},
	} {
		if opt.s == "" {
			continue
		}
		if reopen != nil {
			fmt.Fprintf(os.Stderr, "only one of -doh, -dot, -udp, and -tcp may be given\n")
			os.Exit(1)
		}
		open, address := opt.f, opt.s
		reopen = func() (net.Addr, net.PacketConn, error) { return open(address) }
	}
	if reopen == nil {
		fmt.Fprintf(os.Stderr, "one of -doh, -dot, -udp, or -tcp is required\n")
		os.Exit(1)
	}

	// Over TCP a query's EDNS "UDP payload size" is irrelevant (TCP carries
	// up to 64 KiB), so advertise the max to permit a large server -mtu; and
	// carry upstream data in an EDNS option (upstreamMTU) instead of the
	// ~132 B QNAME. Other transports keep the modest, QNAME-bound defaults.
	ednsPayloadClass := uint16(4096)
	uploadInEDNS := false
	// QNAME-bound upstream MTU: clientid + padding length prefix + padding + data length prefix.
	upstreamMTU := dnsNameCapacity(domain) - 8 - 1 - numPadding - 1
	if tcpAddr != "" {
		ednsPayloadClass = 65535
		// Default (tcp-upstream-mtu 0) keeps the robust QNAME-bound
		// upstream. Only switch to the EDNS carrier when explicitly
		// asked, since large upstream queries can desync some middlebox
		// DNS-over-TCP proxies.
		if tcpUpstreamMTU > 0 {
			if err := validateUpstreamMTU(tcpUpstreamMTU); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			uploadInEDNS = true
			upstreamMTU = tcpUpstreamMTU
		}
	}
	for {
		// Retry initial outages as well as failures after a live session.
		var remoteAddr net.Addr
		var pconn net.PacketConn
		backoff := time.Second
		for {
			remoteAddr, pconn, err = reopen()
			if err == nil {
				break
			}
			log.Printf("opening transport: %v", err)
			time.Sleep(backoff)
			if backoff < 8*time.Second {
				backoff *= 2
			}
		}
		dc := NewDNSPacketConn(pconn, remoteAddr, domain, ednsPayloadClass, uploadInEDNS)
		err = run(pubkey, domain, localAddr, remoteAddr, dc, upstreamMTU)
		dc.Close()
		dc.workers.Wait()
		if !errors.Is(err, errSessionLost) {
			log.Fatal(err)
		}
		log.Printf("%v; establishing a fresh tunnel", err)
		time.Sleep(time.Second)
	}
}
