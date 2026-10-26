// dnstt-server is the server end of a DNS tunnel.
//
// Usage:
//
//	dnstt-server -gen-key [-privkey-file PRIVKEYFILE] [-pubkey-file PUBKEYFILE]
//	dnstt-server -udp ADDR [-privkey PRIVKEY|-privkey-file PRIVKEYFILE] DOMAIN UPSTREAMADDR
//
// Example:
//
//	dnstt-server -gen-key -privkey-file server.key -pubkey-file server.pub
//	dnstt-server -udp :53 -privkey-file server.key t.example.com 127.0.0.1:8000
//
// To generate a persistent server private key, first run with the -gen-key
// option. By default the generated private and public keys are printed to
// standard output. To save them to files instead, use the -privkey-file and
// -pubkey-file options.
//
//	dnstt-server -gen-key
//	dnstt-server -gen-key -privkey-file server.key -pubkey-file server.pub
//
// You can give the server's private key as a file or as a hex string.
//
//	-privkey-file server.key
//	-privkey 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
//
// The -udp option controls the address that will listen for incoming DNS
// queries.
//
// The -mtu option controls the maximum size of response UDP payloads.
// Queries that do not advertise requester support for responses of at least
// this size at least this size will be responded to with a FORMERR. The default
// value is maxUDPPayload.
//
// DOMAIN is the root of the DNS zone reserved for the tunnel. See README for
// instructions on setting it up.
//
// UPSTREAMADDR is the TCP address to which incoming tunnelled streams will be
// forwarded.
package main

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/diagnostics"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/fairqueue"
	"www.bamsoftware.com/git/dnstt.git/noise"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

var health = diagnostics.New("server")

const (
	// turboBuild identifies the fork build in logs; bump on behavior change.
	turboBuild = "2026-10-24-auditfix2"

	// smux streams will be closed after this much time without receiving data.
	idleTimeout = 2 * time.Minute

	// How to set the TTL field in Answer resource records.
	responseTTL = 60

	// How long we may wait for downstream data before sending an empty
	// response. If another query comes in while we are waiting, we'll send
	// an empty response anyway and restart the delay timer for the next
	// response.
	//
	// This number should be less than 2 seconds, which in 2019 was reported
	// to be the query timeout of the Quad9 DoH server.
	// https://dnsencryption.info/imc19-doe.html Section 4.2, Finding 2.4
	// How long to wait for a TCP connection to upstream to be established.
	upstreamDialTimeout = 30 * time.Second
)

// Tuning knobs, defaulted to the proven values and set from flags in main().
var (
	// How long the server may hold a query waiting for the first downstream
	// packet before answering empty. 100ms (~1 RTT) over the reliable
	// DNS-over-TCP path; 1s (upstream default) only added idle latency. Keep
	// under ~2s (historical resolver query timeout). See -max-response-delay.
	maxResponseDelay = 100 * time.Millisecond

	// KCP send window in segments (downstream throughput vs. bufferbloat).
	// See -kcp-window. Default 128 (~180 KB with a 1400 B segment).
	kcpWindow = 65535

	// Cap on the KCP segment size regardless of -mtu; large responses are
	// built by bundling many segments (KCP itself rejects very large MTUs).
	// See -kcp-mtu-cap.
	kcpMTUCap = 1400

	// smux per-stream buffer bytes. See -stream-buffer.
	streamBuffer = 16 * 1024 * 1024

	// Fair download scheduler knobs (see -fair-download and fairqueue.Config).
	fairDownload      = false
	fairChunk         = 4096
	fairIdle          = 100 * time.Millisecond
	fairWriteTimeout  = 500 * time.Millisecond
	fairMaxBurst      = 8
	fairMaxSparse     = 64
	fairSubmitTimeout = time.Duration(0)
	fairLinger        = 250 * time.Millisecond

	// Throughput tunables. All default to current behavior (off/unchanged).
	tcpSockBuf    = 0
	smuxRecvBuf   = 32 * 1024 * 1024 // smux default session-wide receive buffer
	kcpACKNoDelay = false

	// Per-listener response cap for DNS-over-TCP. 0 means use -mtu.
	// See -tcp-mtu. Lets -udp stay at 1232 (no fragmentation) while -tcp
	// uses 16384 (download lever) on one process.
	tcpMtuOverride = 0
)

var (
	// We don't send UDP payloads larger than this, in an attempt to avoid
	// network-layer fragmentation. 1280 is the minimum IPv6 MTU, 40 bytes
	// is the size of an IPv6 header (though without any extension headers),
	// and 8 bytes is the size of a UDP header.
	//
	// Control this value with the -mtu command-line option.
	//
	// https://dnsflagday.net/2020/#message-size-considerations
	// "An EDNS buffer size of 1232 bytes will avoid fragmentation on nearly
	// all current networks."
	//
	// On 2020-04-19, the Quad9 resolver was seen to have a UDP payload size
	// of 1232. Cloudflare's was 1452, and Google's was 4096.
	maxUDPPayload = 1280 - 40 - 8
)

// base32Encoding is a base32 encoding without padding.
var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// logThrottleLast bounds scan-triggered log spam: :53 is internet-facing,
// so background scanners constantly trigger NXDOMAIN/FORMERR/BADVERS.
// Keys are fixed call-site strings (the map cannot grow); each key logs
// its first occurrence and then at most one line per interval.
var (
	logThrottleMu   sync.Mutex
	logThrottleLast = make(map[string]time.Time)
)

// logThrottled logs at most once per interval per key. Live debugging is
// unaffected: the first occurrence always logs immediately.
func logThrottled(key string, interval time.Duration, format string, args ...interface{}) {
	now := time.Now()
	logThrottleMu.Lock()
	if last, ok := logThrottleLast[key]; ok && now.Sub(last) < interval {
		logThrottleMu.Unlock()
		return
	}
	logThrottleLast[key] = now
	logThrottleMu.Unlock()
	log.Printf(format, args...)
}

// generateKeypair generates a private key and the corresponding public key. If
// privkeyFilename and pubkeyFilename are respectively empty, it prints the
// corresponding key to standard output; otherwise it saves the key to the given
// file name. The private key is saved with mode 0400 and the public key is
// saved with 0666 (before umask). In case of any error, it attempts to delete
// any files it has created before returning.
func generateKeypair(privkeyFilename, pubkeyFilename string) (err error) {
	// Filenames to delete in case of error (avoid leaving partially written
	// files).
	var toDelete []string
	defer func() {
		for _, filename := range toDelete {
			fmt.Fprintf(os.Stderr, "deleting partially written file %s\n", filename)
			if closeErr := os.Remove(filename); closeErr != nil {
				fmt.Fprintf(os.Stderr, "cannot remove %s: %v\n", filename, closeErr)
				if err == nil {
					err = closeErr
				}
			}
		}
	}()

	privkey, err := noise.GeneratePrivkey()
	if err != nil {
		return err
	}
	pubkey := noise.PubkeyFromPrivkey(privkey)

	if privkeyFilename != "" {
		// Save the privkey to a file.
		f, err := os.OpenFile(privkeyFilename, os.O_RDWR|os.O_CREATE, 0400)
		if err != nil {
			return err
		}
		toDelete = append(toDelete, privkeyFilename)
		err = noise.WriteKey(f, privkey)
		if err2 := f.Close(); err == nil {
			err = err2
		}
		if err != nil {
			return err
		}
	}

	if pubkeyFilename != "" {
		// Save the pubkey to a file.
		f, err := os.Create(pubkeyFilename)
		if err != nil {
			return err
		}
		toDelete = append(toDelete, pubkeyFilename)
		err = noise.WriteKey(f, pubkey)
		if err2 := f.Close(); err == nil {
			err = err2
		}
		if err != nil {
			return err
		}
	}

	// All good, allow the written files to remain.
	toDelete = nil

	if privkeyFilename != "" {
		fmt.Printf("privkey written to %s\n", privkeyFilename)
	} else {
		fmt.Printf("privkey %x\n", privkey)
	}
	if pubkeyFilename != "" {
		fmt.Printf("pubkey  written to %s\n", pubkeyFilename)
	} else {
		fmt.Printf("pubkey  %x\n", pubkey)
	}

	return nil
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

// handleStream bidirectionally connects a client stream with a TCP socket
// addressed by upstream.
func handleStream(stream *smux.Stream, upstream string, conv uint32, sched *fairqueue.Scheduler) error {
	health.ActiveStreams.Add(1)
	defer health.ActiveStreams.Add(-1)
	dialer := net.Dialer{
		Timeout: upstreamDialTimeout,
	}
	health.UpstreamAttempts.Add(1)
	upstreamConn, err := dialer.Dial("tcp", upstream)
	if err != nil {
		health.UpstreamFailures.Add(1)
		health.Failure("upstream_dial_failed", err)
		return fmt.Errorf("stream %08x:%d connect upstream: %v", conv, stream.ID(), err)
	}
	health.ActiveUpstreams.Add(1)
	defer health.ActiveUpstreams.Add(-1)
	defer upstreamConn.Close()
	upstreamTCPConn := upstreamConn.(*net.TCPConn)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Download (stream <- upstream). When the fair scheduler is enabled it
		// paces this flow into smux in sparse-priority round-robin order with
		// every other flow on the session; otherwise copy greedily as upstream.
		var err error
		if sched != nil {
			f := sched.AddFlow(stream)
			err = sched.Pump(f, upstreamTCPConn)
			if err == io.EOF {
				// Clean end: let queued tail frames flush before FIN,
				// which would otherwise jump them in smux's send queue.
				sched.Linger(f)
			}
			sched.Finish(f)
		} else {
			_, err = io.Copy(stream, upstreamTCPConn)
		}
		if err == io.EOF {
			// smux Stream.Write may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, fairqueue.ErrFlowClosed) && !errors.Is(err, fairqueue.ErrBusy) {
			log.Printf("stream %08x:%d copy stream←upstream: %v", conv, stream.ID(), err)
		}
		upstreamTCPConn.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(upstreamTCPConn, stream)
		if err != nil {
			// A dead session must also release an idle upstream reader.
			upstreamTCPConn.Close()
		}
		if err == io.EOF {
			// smux Stream.WriteTo may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Printf("stream %08x:%d copy upstream←stream: %v", conv, stream.ID(), err)
		}
		upstreamTCPConn.CloseWrite()
	}()
	wg.Wait()

	return nil
}

// acceptStreams wraps a KCP session in a Noise channel and an smux.Session,
// then awaits smux streams. It passes each stream to handleStream.
func acceptStreams(conn *kcp.UDPSession, privkey []byte, upstream string) error {
	// Put a Noise channel on top of the KCP conn.
	handshakeTimer := time.AfterFunc(30*time.Second, func() { conn.Close() })
	rw, err := noise.NewServer(conn, privkey)
	handshakeTimer.Stop()
	if err != nil {
		health.Failure("handshake_failed", err)
		return err
	}
	log.Printf("role=server event=handshake_ready session=%08x", conn.GetConv())

	// Put an smux session on top of the encrypted Noise channel.
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	smuxConfig.MaxStreamBuffer = streamBuffer // upstream dnstt default is 65536
	if smuxRecvBuf < streamBuffer {
		log.Printf("smux-recvbuf %d < stream-buffer %d, clamping up", smuxRecvBuf, streamBuffer)
		smuxRecvBuf = streamBuffer
	}
	smuxConfig.MaxReceiveBuffer = smuxRecvBuf // session-wide; must stay >= streamBuffer
	sess, err := smux.Server(rw, smuxConfig)
	if err != nil {
		return err
	}
	defer sess.Close()
	health.ActiveSessions.Add(1)
	defer health.ActiveSessions.Add(-1)
	log.Printf("role=server event=session_ready session=%08x", conn.GetConv())

	// One fair download scheduler per session (nil = greedy, upstream behavior).
	// No live estimator runs server-side, so its CAKE shaper stays unlimited
	// (DRR++ fairness + Cobalt/overlimit bounds, no ceiling).
	var sched *fairqueue.Scheduler
	if fairDownload {
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
					log.Printf("fair download stats: sparse=%d bulk=%d demoted=%d timedout=%d dropped=%d queued=%d",
						st.SparseTurns, st.BulkTurns, st.Demoted, st.TimedOut, st.Dropped, sched.QueuedLen())
				}
			}
		}()
	}

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		log.Printf("begin stream %08x:%d", conn.GetConv(), stream.ID())
		go func() {
			defer func() {
				log.Printf("end stream %08x:%d", conn.GetConv(), stream.ID())
				stream.Close()
			}()
			err := handleStream(stream, upstream, conn.GetConv(), sched)
			if err != nil {
				log.Printf("stream %08x:%d handleStream: %v", conn.GetConv(), stream.ID(), err)
			}
		}()
	}
}

// acceptSessions listens for incoming KCP connections and passes them to
// acceptStreams.
func acceptSessions(ln *kcp.Listener, privkey []byte, mtu int, upstream string) error {
	for {
		conn, err := ln.AcceptKCP()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		log.Printf("begin session %08x", conn.GetConv())
		// Permit coalescing the payloads of consecutive sends.
		conn.SetStreamMode(true)
		// Disable the dynamic congestion window (limit only by the
		// maximum of local and remote static windows).
		conn.SetNoDelay(
			0, // default nodelay
			0, // default interval
			0, // default resend
			1, // nc=1 => congestion window off
		)
		if kcpACKNoDelay {
			conn.SetACKNoDelay(true)
		}
		conn.SetWindowSize(kcpWindow, kcpWindow)
		if !conn.SetMtu(mtu) {
			conn.Close()
			return fmt.Errorf("session %08x: KCP refused MTU %d", conn.GetConv(), mtu)
		}
		go func() {
			defer func() {
				log.Printf("end session %08x", conn.GetConv())
				conn.Close()
			}()
			err := acceptStreams(conn, privkey, upstream)
			if err != nil && !errors.Is(err, io.ErrClosedPipe) {
				log.Printf("session %08x acceptStreams: %v", conn.GetConv(), err)
			}
		}()
	}
}

// nextPacket reads the next length-prefixed packet from r, ignoring padding. It
// returns a nil error only when a packet was read successfully. It returns
// io.EOF only when there were 0 bytes remaining to read from r. It returns
// io.ErrUnexpectedEOF when EOF occurs in the middle of an encoded packet.
//
// The prefixing scheme is as follows. A length prefix L < 0xe0 means a data
// packet of L bytes. A length prefix L >= 0xe0 means padding of L - 0xe0 bytes
// (not counting the length of the length prefix itself).
func nextPacket(r *bytes.Reader) ([]byte, error) {
	// Convert io.EOF to io.ErrUnexpectedEOF.
	eof := func(err error) error {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}

	for {
		prefix, err := r.ReadByte()
		if err != nil {
			// We may return a real io.EOF only here.
			return nil, err
		}
		if prefix >= 224 {
			paddingLen := prefix - 224
			_, err := io.CopyN(ioutil.Discard, r, int64(paddingLen))
			if err != nil {
				return nil, eof(err)
			}
		} else {
			p := make([]byte, int(prefix))
			_, err = io.ReadFull(r, p)
			return p, eof(err)
		}
	}
}

// responseFor constructs a response dns.Message that is appropriate for query.
// Along with the dns.Message, it returns the query's decoded data payload
// and the client's advertised EDNS payload size (0 if no OPT). If
// the returned dns.Message is nil, it means that there should be no response to
// this query. If the returned dns.Message has an Rcode() of dns.RcodeNoError,
// the message is a candidate for for carrying downstream data in a TXT record.
//
// serverMax is this listener's maximum response size (per-listener -mtu /
// -tcp-mtu). Smaller caps are supported only if a whole configured KCP
// segment fits even with the longest query name. Otherwise return FORMERR
// before admitting upstream packets: KCP cannot negotiate a smaller MTU here.
func responseFor(query *dns.Message, domain dns.Name, serverMax int) (*dns.Message, []byte, int) {
	resp := &dns.Message{
		ID:       query.ID,
		Flags:    0x8000, // QR = 1, RCODE = no error
		Question: query.Question,
	}

	if query.Flags&0x8000 != 0 {
		// QR != 0, this is not a query. Don't even send a response.
		return nil, nil, 0
	}

	// Check for EDNS(0) support. Include our own OPT RR only if we receive
	// one from the requester.
	// https://tools.ietf.org/html/rfc6891#section-6.1.1
	// "Lack of presence of an OPT record in a request MUST be taken as an
	// indication that the requester does not implement any part of this
	// specification and that the responder MUST NOT include an OPT record
	// in its response."
	payloadSize := 0
	hasOPT := false
	for _, rr := range query.Additional {
		if rr.Type != dns.RRTypeOPT {
			continue
		}
		if len(resp.Additional) != 0 {
			// https://tools.ietf.org/html/rfc6891#section-6.1.1
			// "If a query message with more than one OPT RR is
			// received, a FORMERR (RCODE=1) MUST be returned."
			resp.Flags |= dns.RcodeFormatError
			logThrottled("formerr-multi-opt", time.Minute, "FORMERR: more than one OPT RR")
			return resp, nil, payloadSize
		}
		resp.Additional = append(resp.Additional, dns.RR{
			Name:  dns.Name{},
			Type:  dns.RRTypeOPT,
			Class: 4096, // responder's UDP payload size
			TTL:   0,
			Data:  []byte{},
		})
		additional := &resp.Additional[0]

		version := (rr.TTL >> 16) & 0xff
		if version != 0 {
			// https://tools.ietf.org/html/rfc6891#section-6.1.1
			// "If a responder does not implement the VERSION level
			// of the request, then it MUST respond with
			// RCODE=BADVERS."
			resp.Flags |= dns.ExtendedRcodeBadVers & 0xf
			additional.TTL = (dns.ExtendedRcodeBadVers >> 4) << 24
			logThrottled("badvers", time.Minute, "BADVERS: EDNS version %d != 0", version)
			return resp, nil, payloadSize
		}

		payloadSize = int(rr.Class)
		hasOPT = true
	}
	advertised := payloadSize
	// We will return RcodeFormatError if the requester does not support
	// EDNS(0) with at least the protocol minimum, but first, check the
	// name in order to set the AA bit properly. Smaller advertised caps must
	// still carry this listener's configured KCP segment.

	// There must be exactly one question.
	if len(query.Question) != 1 {
		resp.Flags |= dns.RcodeFormatError
		logThrottled("formerr-questions", time.Minute, "FORMERR: too few or too many questions (%d)", len(query.Question))
		return resp, nil, advertised
	}
	question := query.Question[0]
	// Check the name to see if it ends in our chosen domain, and extract
	// all that comes before the domain if it does. If it does not, we will
	// return RcodeNameError below, but prefer to return RcodeFormatError
	// for payload size if that applies as well.
	prefix, ok := question.Name.TrimSuffix(domain)
	if !ok {
		// Not a name we are authoritative for.
		resp.Flags |= dns.RcodeNameError
		logThrottled("nxdomain-auth", time.Minute, "NXDOMAIN: not authoritative for %s", question.Name)
		return resp, nil, advertised
	}
	resp.Flags |= 0x0400 // AA = 1

	if query.Opcode() != 0 {
		// We don't support OPCODE != QUERY.
		resp.Flags |= dns.RcodeNotImplemented
		logThrottled("notimpl-opcode", time.Minute, "NOTIMPL: unrecognized OPCODE %d", query.Opcode())
		return resp, nil, advertised
	}

	if question.Type != dns.RRTypeTXT {
		// We only support QTYPE == TXT.
		resp.Flags |= dns.RcodeNameError
		// No log message here; it's common for recursive resolvers to
		// send NS or A queries when the client only asked for a TXT. I
		// suspect this is related to QNAME minimization, but I'm not
		// sure. https://tools.ietf.org/html/rfc7816
		// log.Printf("NXDOMAIN: QTYPE %d != TXT", question.Type)
		return resp, nil, advertised
	}

	encoded := bytes.ToUpper(bytes.Join(prefix, nil))
	payload := make([]byte, base32Encoding.DecodedLen(len(encoded)))
	n, err := base32Encoding.Decode(payload, encoded)
	if err != nil {
		// Base32 error, make like the name doesn't exist.
		resp.Flags |= dns.RcodeNameError
		logThrottled("nxdomain-base32", time.Minute, "NXDOMAIN: base32 decoding: %v", err)
		return resp, nil, advertised
	}
	payload = payload[:n]

	// Require EDNS(0) with at least the protocol minimum of 512 octets
	// (RFC 6891), and enough space for a complete configured segment.
	// Smaller caps cannot be fixed by slicing the encoded DNS response.
	// Without an OPT RR
	// (hasOPT false, advertised 0) there is no negotiated size to cap to.
	// https://tools.ietf.org/html/rfc6891#section-7
	if !hasOPT || advertised < 512 {
		resp.Flags |= dns.RcodeFormatError
		logThrottled("formerr-plsize", time.Minute, "FORMERR: requester payload size %d is too small (minimum %d)", advertised, 512)
		return resp, nil, advertised
	}
	if advertised < serverMax && advertised < minimumResponseCap(serverMax) {
		resp.Flags |= dns.RcodeFormatError
		logThrottled("formerr-segment-size", time.Minute, "FORMERR: advertised response cap %d cannot carry this listener's KCP segment; use separate -mtu/-tcp-mtu limits", advertised)
		return resp, nil, advertised
	}

	return resp, payload, advertised
}

// ednsDataOptionCode matches the client: in -tcp mode the upstream data packet
// is carried in an EDNS(0) option with this private-use code rather than in the
// QNAME, which lifts the upload per-query cap from ~132 B to the KCP MTU.
const ednsDataOptionCode = 0xfde9

// maxEDNSPacket bounds a single upstream packet carried in our EDNS option.
// The client validates -tcp-upstream-mtu to 80..1500 (kcp-go session cap), so
// anything larger cannot be a legal KCP segment and is dropped before KCP ever
// sees it. The QNAME path is already bounded to <224 bytes by its length
// prefix; this is the EDNS equivalent.
const maxEDNSPacket = 1500

// ednsDataOption returns the upstream packet carried in our EDNS option of the
// query's OPT RR, or nil if absent. Options are code(2)+len(2)+value, per RFC 6891.
func ednsDataOption(query *dns.Message) []byte {
	for _, rr := range query.Additional {
		if rr.Type != dns.RRTypeOPT {
			continue
		}
		data := rr.Data
		for len(data) >= 4 {
			code := binary.BigEndian.Uint16(data[0:2])
			ln := int(binary.BigEndian.Uint16(data[2:4]))
			if 4+ln > len(data) {
				break
			}
			val := data[4 : 4+ln]
			data = data[4+ln:]
			if code == ednsDataOptionCode {
				return val
			}
		}
	}
	return nil
}

// record represents a DNS message appropriate for a response to a previously
// received query, along with metadata necessary for sending the response.
// recvLoop sends instances of record to sendLoop via a channel. sendLoop
// receives instances of record and may fill in the message's Answer section
// before sending it.
type record struct {
	Resp     *dns.Message
	Addr     net.Addr
	ClientID turbotunnel.ClientID
	// Advertised is the requester's EDNS payload size for this query. sendLoop
	// caps each response to min(serverMax, Advertised) so one serverMax can
	// serve 4096-octet UDP clients and 65535-octet TCP clients at once.
	Advertised int
}

// recvLoop repeatedly calls dnsConn.ReadFrom, extracts the packets contained in
// the incoming DNS queries, and puts them on ttConn's incoming queue. Whenever
// a query calls for a response, constructs a partial response and passes it to
// sendLoop over ch. ch is never closed while this sender lives: run() waits
// for recvLoop to return (via dnsConn.Close unblocking ReadFrom) before
// closing ch, so the send below cannot panic on a closed channel.
// serverMax is this listener's maximum response size.
func recvLoop(domain dns.Name, dnsConn net.PacketConn, ttConn *turbotunnel.QueuePacketConn, ch chan<- *record, serverMax int) error {
	return recvLoopContext(domain, dnsConn, ttConn, ch, serverMax, nil)
}

func recvLoopContext(domain dns.Name, dnsConn net.PacketConn, ttConn *turbotunnel.QueuePacketConn, ch chan<- *record, serverMax int, done <-chan struct{}) error {
	reliable := false
	if transport, ok := dnsConn.(interface{ Reliable() bool }); ok {
		reliable = transport.Reliable()
	}
	// Parsed names and RDATA own their storage, and QueueIncoming copies
	// packets. No queued response retains this scratch buffer.
	var buf [65536]byte
	for {
		// 64 KiB receive buffer: the ceiling for framed transports (TCP
		// carries up to 64 KiB queries). Over UDP the transport still
		// returns small datagrams, so this only raises the cap. Oversize
		// EDNS upload packets are bounded by maxEDNSPacket below and the
		// QNAME path by its <224 length prefix; neither is trusted blindly.
		n, addr, err := dnsConn.ReadFrom(buf[:])
		if err != nil {
			return err
		}

		// Got a UDP packet. Try to parse it as a DNS message.
		query, err := dns.MessageFromWireFormat(buf[:n])
		if err != nil {
			logThrottled("parse-query", time.Minute, "cannot parse DNS query: %v", err)
			continue
		}

		resp, payload, advertised := responseFor(&query, domain, serverMax)
		// Extract the ClientID from the payload.
		var clientID turbotunnel.ClientID
		idLen := copy(clientID[:], payload)
		payload = payload[idLen:]
		if idLen == len(clientID) {
			// Discard padding and pull out the packets contained in
			// the payload.
			r := bytes.NewReader(payload)
			for {
				p, err := nextPacket(r)
				if err != nil {
					break
				}
				// Feed the incoming packet to KCP.
				ttConn.QueueIncoming(p, clientID)
			}
			// Also accept an upstream packet carried in our EDNS
			// option (-tcp clients upload there, not in the QNAME).
			if ep := ednsDataOption(&query); len(ep) > 0 {
				if len(ep) > maxEDNSPacket {
					logThrottled("drop-edns-oversize", time.Minute, "dropping %d-byte EDNS upload packet, exceeds %d", len(ep), maxEDNSPacket)
				} else {
					ttConn.QueueIncoming(ep, clientID)
				}
			}
		} else {
			// Payload is not long enough to contain a ClientID.
			if resp != nil && resp.Rcode() == dns.RcodeNoError {
				resp.Flags |= dns.RcodeNameError
				logThrottled("nxdomain-short-id", time.Minute, "NXDOMAIN: %d bytes are too short to contain a ClientID", idLen)
			}
		}
		// If a response is called for, pass it to sendLoop via the channel.
		if resp != nil {
			if reliable {
				// TCP already provides backpressure. Losing a response here
				// leaves the client waiting for a query we processed.
				select {
				case ch <- &record{resp, addr, clientID, advertised}:
				case <-done:
					return net.ErrClosed
				}
				continue
			}
			select {
			case ch <- &record{resp, addr, clientID, advertised}:
			default:
			}
		}
	}
}

// sendLoop repeatedly receives records from ch. Those that represent an error
// response, it sends on the network immediately. Those that represent a
// response capable of carrying data, it packs full of as many packets as will
// fit while keeping the total size under the per-response budget, then sends
// it. The budget is min(serverMax, rec.Advertised): one serverMax serves both
// small UDP clients and large TCP clients. maxEncodedPayload is the
// precomputed budget for serverMax itself.
func sendLoop(dnsConn net.PacketConn, ttConn *turbotunnel.QueuePacketConn, ch <-chan *record, serverMax, maxEncodedPayload int) error {
	// Budget cache by response cap. Distinct caps are few (4096, 65535,
	// serverMax), so memoizing avoids a binary-search WireFormat probe per
	// response at high qps.
	budgetCache := map[int]int{serverMax: maxEncodedPayload}
	budgetFor := func(cap int) int {
		if cap >= serverMax {
			return maxEncodedPayload
		}
		if v, ok := budgetCache[cap]; ok {
			return v
		}
		v := computeMaxEncodedPayload(cap)
		budgetCache[cap] = v
		return v
	}
	var nextRec *record
	// TXT encoding owns its output, so bundle assembly scratch can be
	// reused without aliasing a response or the transport's write buffer.
	var payload bytes.Buffer
	for {
		rec := nextRec
		nextRec = nil

		if rec == nil {
			var ok bool
			rec, ok = <-ch
			if !ok {
				break
			}
		}

		if rec.Resp.Rcode() == dns.RcodeNoError && len(rec.Resp.Question) == 1 {
			// If it's a non-error response, we can fill the Answer
			// section with downstream packets.

			// Any changes to how responses are built need to happen
			// also in computeMaxEncodedPayload.
			rec.Resp.Answer = []dns.RR{
				{
					Name:  rec.Resp.Question[0].Name,
					Type:  rec.Resp.Question[0].Type,
					Class: rec.Resp.Question[0].Class,
					TTL:   responseTTL,
					Data:  nil, // will be filled in below
				},
			}

			respCap := responseCap(serverMax, rec.Advertised)
			payload.Reset()
			limit := budgetFor(respCap)
			// We loop and bundle as many packets from OutgoingQueue
			// into the response as will fit. Any packet that would
			// overflow the capacity of the DNS response, we stash
			// to be bundled into a future response.
			timer := time.NewTimer(maxResponseDelay)
			for {
				var p []byte
				outgoing, unstash := ttConn.ExistingQueues(rec.ClientID)
				// Prioritize taking a packet first from the
				// stash, then from the outgoing queue, then
				// finally check for the expiration of the timer
				// or for a receive on ch (indicating a new
				// query that we must respond to).
				select {
				case p = <-unstash:
				default:
					select {
					case p = <-unstash:
					case p = <-outgoing:
					default:
						select {
						case p = <-unstash:
						case p = <-outgoing:
						case <-timer.C:
						case nextRec = <-ch:
						}
					}
				}
				// We wait for the first packet in a bundle
				// only. The second and later packets must be
				// immediately available or they will be omitted
				// from this bundle.
				// Stop+drain before Reset: correct under both pre-1.23
				// and post-1.23 timer semantics (go.mod pins 1.21).
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(0)

				if len(p) == 0 {
					// timer expired or receive on ch, we
					// are done with this response.
					break
				}

				limit -= 2 + len(p)
				if limit < 0 {
					// Stash this packet to send in the next
					// response. Never emit a partial KCP segment.
					ttConn.Stash(p, rec.ClientID)
					break
				}
				if int(uint16(len(p))) != len(p) {
					// Cannot frame it; drop so one oversize KCP segment
					// can't crash the whole server (all sessions).
					// Log unthrottled: reaching here needs a local KCP
					// misconfiguration, never remote input.
					log.Printf("sendLoop: dropping %d-byte packet, exceeds response framing", len(p))
					break
				}
				binary.Write(&payload, binary.BigEndian, uint16(len(p)))
				payload.Write(p)
			}
			timer.Stop()

			rec.Resp.Answer[0].Data = dns.EncodeRDataTXT(payload.Bytes())
		}

		buf, err := rec.Resp.WireFormat()
		if err != nil {
			log.Printf("resp WireFormat: %v", err)
			continue
		}
		// Truncate if necessary to this query's cap:
		// min(serverMax, advertised). Error responses (FORMERR/NXDOMAIN)
		// carry no bundle and have Advertised 0 when no OPT was sent, so
		// they cap at serverMax.
		// https://tools.ietf.org/html/rfc1035#section-4.1.1
		respCap := responseCap(serverMax, rec.Advertised)
		if len(buf) > respCap {
			// Truncate at record boundaries, never in the middle of TXT
			// RDATA (which makes the entire DNS message unparseable).
			rec.Resp.Flags |= 0x0200 // TC = 1
			rec.Resp.Answer = nil
			rec.Resp.Authority = nil
			buf, err = rec.Resp.WireFormat()
			if err != nil || len(buf) > respCap {
				// Malformed requests may contain hundreds of questions. Do
				// not echo an oversized question section or let it stop all
				// clients' responses; emit a complete truncated header.
				rec.Resp.Question = nil
				rec.Resp.Additional = nil
				buf, err = rec.Resp.WireFormat()
				if err != nil {
					log.Printf("truncated response WireFormat: %v", err)
					continue
				}
			}
		}

		// Now we actually send the message as a UDP packet.
		_, err = dnsConn.WriteTo(buf, rec.Addr)
		if err != nil {
			// net.ErrClosed means we'll never be able to send on
			// dnsConn, so terminate the loop. Treat all other
			// errors as temporary and simply log them: a failed
			// sendto (e.g. conntrack-full firewall deny) must not
			// wedge the server into running-but-not-responding.
			// KCP retransmits what the client still needs.
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			health.Failure("dns_reply_failed", err)
			continue
		}
	}
	return nil
}

// computeMaxEncodedPayload computes the maximum amount of downstream TXT RR
// data that keep the overall response size less than limit, in the
// worst case when the response answers a query that has a maximum-length name
// in its Question section. Returns 0 in the case that no amount of data makes
// the overall response size small enough.
//
// This function needs to be kept in sync with sendLoop with regard to how it
// builds candidate responses.
func computeMaxEncodedPayload(limit int) int {
	// 64+64+64+62 octets, needs to be base32-decodable.
	maxLengthName, err := dns.NewName([][]byte{
		[]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		[]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		[]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
		[]byte("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"),
	})
	if err != nil {
		panic(err)
	}
	{
		// Compute the encoded length of maxLengthName and that its
		// length is actually at the maximum of 255 octets.
		n := 0
		for _, label := range maxLengthName {
			n += len(label) + 1
		}
		n += 1 // For the terminating null label.
		if n != 255 {
			panic(fmt.Sprintf("max-length name is %d octets, should be %d %s", n, 255, maxLengthName))
		}
	}

	queryLimit := uint16(limit)
	if int(queryLimit) != limit {
		queryLimit = 0xffff
	}
	query := &dns.Message{
		Question: []dns.Question{
			{
				Name:  maxLengthName,
				Type:  dns.RRTypeTXT,
				Class: dns.RRTypeTXT,
			},
		},
		// EDNS(0)
		Additional: []dns.RR{
			{
				Name:  dns.Name{},
				Type:  dns.RRTypeOPT,
				Class: queryLimit, // requester's UDP payload size
				TTL:   0,          // extended RCODE and flags
				Data:  []byte{},
			},
		},
	}
	resp, _, _ := responseFor(query, dns.Name([][]byte{}), limit)
	// As in sendLoop.
	resp.Answer = []dns.RR{
		{
			Name:  query.Question[0].Name,
			Type:  query.Question[0].Type,
			Class: query.Question[0].Class,
			TTL:   responseTTL,
			Data:  nil, // will be filled in below
		},
	}

	// Binary search to find the maximum payload length that does not result
	// in a wire-format message whose length exceeds the limit.
	low := 0
	high := 32768
	for low+1 < high {
		mid := (low + high) / 2
		resp.Answer[0].Data = dns.EncodeRDataTXT(make([]byte, mid))
		buf, err := resp.WireFormat()
		if err != nil {
			panic(err)
		}
		if len(buf) <= limit {
			low = mid
		} else {
			high = mid
		}
	}

	return low
}

var minimumResponseCaps sync.Map

func responseCap(serverMax, advertised int) int {
	// RFC 6891 treats advertised sizes below 512 as 512. Such queries
	// still receive FORMERR from admission, but their error response needs
	// enough space for a DNS header and must never crash/stop the send loop.
	if advertised < 512 {
		advertised = 512
	}
	if advertised > serverMax {
		return serverMax
	}
	return advertised
}

// Compute once per listener configuration, not once per wire query. The KCP
// segment cap is included in the key for callers using different settings.
func minimumResponseCap(serverMax int) int {
	key := [2]int{serverMax, kcpMTUCap}
	if value, ok := minimumResponseCaps.Load(key); ok {
		return value.(int)
	}
	mtu := computeMaxEncodedPayload(serverMax) - 2
	if mtu > kcpMTUCap {
		mtu = kcpMTUCap
	}
	low, high := 511, serverMax
	for low+1 < high {
		mid := low + (high-low)/2
		if computeMaxEncodedPayload(mid) >= mtu+2 {
			high = mid
		} else {
			low = mid
		}
	}
	minimumResponseCaps.Store(key, high)
	return high
}

func run(privkey []byte, domain dns.Name, upstream string, dnsConn net.PacketConn, serverMax int) error {
	stopHealth := health.Start()
	defer stopHealth()
	log.Printf("role=server event=session_config transport=%T response_cap=%d kcp_window=%d fair_download=%t stream_buffer=%d smux_recvbuf=%d", dnsConn, serverMax, kcpWindow, fairDownload, streamBuffer, smuxRecvBuf)
	defer dnsConn.Close()

	if serverMax <= 0 {
		serverMax = maxUDPPayload
	}

	log.Printf("pubkey %x", noise.PubkeyFromPrivkey(privkey))

	// We have a variable amount of room in which to encode downstream
	// packets in each response, because each response must contain the
	// query's Question section, which is of variable length. But we cannot
	// give dynamic packet size limits to KCP; the best we can do is set a
	// maximum which no KCP segment will exceed. We choose that maximum to
	// keep the response under serverMax (this listener's -mtu/-tcp-mtu),
	// even in the worst case of a maximum-length name in the query's
	// Question section. Each response is additionally capped to
	// min(serverMax, client advertised) in sendLoop, so one serverMax can
	// serve compatible smaller-cap clients and large TCP clients at once.
	maxEncodedPayload := computeMaxEncodedPayload(serverMax)
	// 2 bytes accounts for a packet length prefix.
	mtu := maxEncodedPayload - 2
	// KCP segment size. Large responses (big serverMax, e.g. DNS-over-TCP)
	// are built by BUNDLING many KCP segments into one response (see
	// sendLoop), so the per-segment MTU stays modest. KCP itself refuses
	// very large MTUs, so cap it while still allowing a large bundle.
	if mtu > kcpMTUCap {
		mtu = kcpMTUCap
	}
	if mtu < 80 {
		if mtu < 0 {
			mtu = 0
		}
		return fmt.Errorf("maximum payload size of %d leaves only %d bytes for payload", serverMax, mtu)
	}
	log.Printf("effective MTU %d (response bundle up to %d bytes, cap %d)", mtu, maxEncodedPayload, serverMax)

	// Start up the virtual PacketConn for turbotunnel.
	ttConn := turbotunnel.NewQueuePacketConn(turbotunnel.DummyAddr{}, idleTimeout*2)
	ln, err := kcp.ServeConn(nil, 0, 0, ttConn)
	if err != nil {
		return fmt.Errorf("opening KCP listener: %v", err)
	}
	defer ln.Close()
	defer ttConn.Close()

	// We run acceptSessions, sendLoop, and recvLoop concurrently. In normal
	// operation none of them returns, so the first return means the listener
	// is half-dead and the process must exit (supervised restart) rather
	// than run deaf. Shutdown order matters: ch has a live sender
	// (recvLoop), so it must only be closed AFTER recvLoop has returned;
	// closing it earlier panics the sender ("send on closed channel").
	// We therefore unblock recvLoop via dnsConn.Close, wait for it, then
	// close ch to unblock sendLoop.
	doneChan := make(chan struct{})
	var doneOnce sync.Once
	done := func() { doneOnce.Do(func() { close(doneChan) }) }
	errCh := make(chan error, 3)
	recvDone := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		defer done()
		err := acceptSessions(ln, privkey, mtu, upstream)
		if err != nil {
			log.Printf("acceptSessions: %v", err)
		}
		errCh <- err
	}()

	ch := make(chan *record, 100)

	// We could run multiple copies of sendLoop; that would allow more time
	// for each response to collect downstream data before being evicted by
	// another response that needs to be sent.
	go func() {
		defer wg.Done()
		defer done()
		err := sendLoop(dnsConn, ttConn, ch, serverMax, maxEncodedPayload)
		if err != nil {
			log.Printf("sendLoop: %v", err)
		}
		errCh <- err
	}()

	go func() {
		defer wg.Done()
		defer done()
		defer close(recvDone)
		err := recvLoopContext(domain, dnsConn, ttConn, ch, serverMax, doneChan)
		if err != nil {
			log.Printf("recvLoop: %v", err)
		}
		errCh <- err
	}()

	<-doneChan
	// Unblock any stragglers: ReadFrom (recvLoop) and AcceptKCP
	// (acceptSessions) observe these closes as errors and return. Second
	// closes via defer are harmless errors, ignored.
	dnsConn.Close()
	ln.Close()
	ttConn.Close()
	// Wait for the producer before closing its channel: the only safe
	// point to close ch. sendLoop terminates on the close (ok=false).
	<-recvDone
	close(ch)
	wg.Wait()
	var firstErr error
	for i := 0; i < 3; i++ {
		if e := <-errCh; e != nil && firstErr == nil {
			firstErr = e
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("listener half-dead: loop exited")
}

func main() {
	var genKey bool
	var privkeyFilename string
	var privkeyString string
	var pubkeyFilename string
	var udpAddr string
	var tcpAddr string

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `Usage:
  %[1]s -gen-key -privkey-file PRIVKEYFILE -pubkey-file PUBKEYFILE
  %[1]s -udp ADDR -privkey-file PRIVKEYFILE DOMAIN UPSTREAMADDR

Example:
  %[1]s -gen-key -privkey-file server.key -pubkey-file server.pub
  %[1]s -udp :53 -privkey-file server.key t.example.com 127.0.0.1:8000

`, os.Args[0])
		flag.PrintDefaults()
	}
	flag.BoolVar(&genKey, "gen-key", false, "generate a server keypair; print to stdout or save to files")
	flag.IntVar(&maxUDPPayload, "mtu", maxUDPPayload, "maximum size of DNS responses (UDP default; TCP uses -tcp-mtu when set)")
	flag.IntVar(&tcpMtuOverride, "tcp-mtu", 0, "maximum size of DNS-over-TCP responses (0 = use -mtu; e.g. -mtu 1232 -tcp-mtu 16384 for dual -udp/-tcp)")
	flag.StringVar(&privkeyString, "privkey", "", fmt.Sprintf("server private key (%d hex digits)", noise.KeyLen*2))
	flag.StringVar(&privkeyFilename, "privkey-file", "", "read server private key from file (with -gen-key, write to file)")
	flag.StringVar(&pubkeyFilename, "pubkey-file", "", "with -gen-key, write server public key to file")
	flag.StringVar(&udpAddr, "udp", "", "UDP address to listen on (one of -udp/-tcp required)")
	flag.StringVar(&tcpAddr, "tcp", "", "plain DNS-over-TCP address to listen on, e.g. :53 (use a large -mtu, e.g. 16384)")
	// --- performance tuning flags ---
	var fKCPWindow, fKCPMTUCap, fStreamBuffer, fFairChunk, fFairMaxBurst, fFairMaxSparse, fTCPSockBuf, fSmuxRecvBuf int
	var fMaxRespDelay, fFairIdle, fFairWriteTimeout, fFairSubmitTimeout, fFairLinger time.Duration
	var fKCPACKNoDelay bool
	var fFairDownload bool
	flag.IntVar(&fKCPWindow, "kcp-window", kcpWindow, "KCP send window in segments (downstream throughput vs. bufferbloat)")
	flag.IntVar(&fKCPMTUCap, "kcp-mtu-cap", kcpMTUCap, "cap on KCP segment size; big responses come from bundling, not big segments")
	flag.DurationVar(&fMaxRespDelay, "max-response-delay", maxResponseDelay, "max time to hold a query for downstream data before answering empty")
	flag.IntVar(&fStreamBuffer, "stream-buffer", streamBuffer, "smux per-stream buffer bytes")
	// Fair download scheduler: paces download flows into smux in sparse-priority
	// round-robin order so a bulk flood (e.g. BitTorrent) can't starve
	// interactive/DNS streams on the shared session. Default off = greedy.
	flag.BoolVar(&fFairDownload, "fair-download", fairDownload, "fair-queue the download path (sparse-priority round-robin) so bulk can't starve interactive")
	flag.IntVar(&fFairChunk, "fair-chunk", fairChunk, "with -fair-download: max bytes served per flow per turn")
	flag.DurationVar(&fFairIdle, "fair-idle", fairIdle, "with -fair-download: idle gap that re-marks a flow sparse (interactive)")
	flag.DurationVar(&fFairWriteTimeout, "fair-write-timeout", fairWriteTimeout, "with -fair-download: per-chunk smux write deadline (yields a stalled flow)")
	flag.IntVar(&fFairMaxBurst, "fair-max-burst", fairMaxBurst, "with -fair-download: consecutive sparse turns before one bulk turn is forced (bulk starvation protection; <=0 = strict sparse)")
	flag.IntVar(&fFairMaxSparse, "fair-max-sparse", fairMaxSparse, "with -fair-download: cap on the sparse list; overflow demotes to round-robin")
	flag.DurationVar(&fFairSubmitTimeout, "fair-submit-timeout", fairSubmitTimeout, "with -fair-download: scheduler starvation timeout (0 = wait through congestion; positive expiry fails the stream)")
	flag.DurationVar(&fFairLinger, "fair-linger", fairLinger, "with -fair-download: after clean pump end, wait for quiet before FIN so it can't jump queued tail data (0 = off)")
	// --- throughput tunables (all default to current behavior) ---
	flag.IntVar(&fTCPSockBuf, "tcp-sockbuf", tcpSockBuf, "with -tcp: kernel socket buffers (SO_RCVBUF/SO_SNDBUF) on accepted port-53 connections, 0 = OS default")
	flag.IntVar(&fSmuxRecvBuf, "smux-recvbuf", smuxRecvBuf, "smux session-wide receive buffer bytes (must stay >= -stream-buffer)")
	flag.BoolVar(&fKCPACKNoDelay, "kcp-acknodelay", kcpACKNoDelay, "flush KCP ACKs immediately instead of batching at the update interval")
	configureLog := diagnostics.RegisterLogFlags()
	flag.Parse()
	if err := configureLog(); err != nil {
		log.Fatal(err)
	}

	// Apply tuning knobs before any QueuePacketConn/KCP listener is created.
	kcpWindow = fKCPWindow
	if kcpWindow < 1 || kcpWindow > 65535 {
		log.Fatal("kcp-window must be 1..65535 (KCP wire limit)")
	}
	kcpMTUCap = fKCPMTUCap
	maxResponseDelay = fMaxRespDelay
	streamBuffer = fStreamBuffer
	fairDownload = fFairDownload
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
	turbotunnel.SetQueueSize(kcpWindow * 2)

	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("dnstt-turbo server build %s", turboBuild)

	if genKey {
		// -gen-key mode.
		if flag.NArg() != 0 || privkeyString != "" || udpAddr != "" || tcpAddr != "" || tcpMtuOverride != 0 {
			flag.Usage()
			os.Exit(1)
		}
		if err := generateKeypair(privkeyFilename, pubkeyFilename); err != nil {
			fmt.Fprintf(os.Stderr, "cannot generate keypair: %v\n", err)
			os.Exit(1)
		}
	} else {
		// Ordinary server mode.
		if flag.NArg() != 2 {
			flag.Usage()
			os.Exit(1)
		}
		domain, err := dns.ParseName(flag.Arg(0))
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid domain %+q: %v\n", flag.Arg(0), err)
			os.Exit(1)
		}
		upstream := flag.Arg(1)
		// We keep upstream as a string in order to eventually pass it
		// to net.Dial in handleStream. But for the sake of displaying
		// an error or warning at startup, rather than only when the
		// first stream occurs, we apply some parsing and name
		// resolution checks here.
		{
			upstreamHost, _, err := net.SplitHostPort(upstream)
			if err != nil {
				// host:port format is required in all cases, so
				// this is a fatal error.
				fmt.Fprintf(os.Stderr, "cannot parse upstream address %+q: %v\n", upstream, err)
				os.Exit(1)
			}
			upstreamIPAddr, err := net.ResolveIPAddr("ip", upstreamHost)
			if err != nil {
				// Failure to resolve the host portion is only a
				// warning. The name will be re-resolved on each
				// net.Dial in handleStream.
				log.Printf("warning: cannot resolve upstream host %+q: %v", upstreamHost, err)
			} else if upstreamIPAddr.IP == nil {
				// Handle the special case of an empty string
				// for the host portion, which resolves to a nil
				// IP. This is a fatal error as we will not be
				// able to dial this address.
				fmt.Fprintf(os.Stderr, "cannot parse upstream address %+q: missing host in address\n", upstream)
				os.Exit(1)
			}
		}

		if udpAddr == "" && tcpAddr == "" {
			fmt.Fprintf(os.Stderr, "at least one of -udp or -tcp is required\n")
			os.Exit(1)
		}

		if pubkeyFilename != "" {
			fmt.Fprintf(os.Stderr, "-pubkey-file may only be used with -gen-key\n")
			os.Exit(1)
		}

		var privkey []byte
		if privkeyFilename != "" && privkeyString != "" {
			fmt.Fprintf(os.Stderr, "only one of -privkey and -privkey-file may be used\n")
			os.Exit(1)
		} else if privkeyFilename != "" {
			var err error
			privkey, err = readKeyFromFile(privkeyFilename)
			if err != nil {
				fmt.Fprintf(os.Stderr, "cannot read privkey from file: %v\n", err)
				os.Exit(1)
			}
		} else if privkeyString != "" {
			var err error
			privkey, err = noise.DecodeKey(privkeyString)
			if err != nil {
				fmt.Fprintf(os.Stderr, "privkey format error: %v\n", err)
				os.Exit(1)
			}
		}
		if len(privkey) == 0 {
			log.Println("generating a temporary one-time keypair")
			log.Println("use the -privkey or -privkey-file option for a persistent server keypair")
			var err error
			privkey, err = noise.GeneratePrivkey()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}

		var dnsConns []net.PacketConn
		var dnsCaps []int
		if udpAddr != "" {
			if maxUDPPayload < 512 || maxUDPPayload > 65535 {
				fmt.Fprintf(os.Stderr, "-mtu %d out of range 512..65535\n", maxUDPPayload)
				os.Exit(1)
			}
			dnsConn, err := net.ListenPacket("udp", udpAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "opening UDP listener: %v\n", err)
				os.Exit(1)
			}
			dnsConns = append(dnsConns, dnsConn)
			dnsCaps = append(dnsCaps, maxUDPPayload)
		}
		if tcpAddr != "" {
			tcpMax := maxUDPPayload
			if tcpMtuOverride > 0 {
				tcpMax = tcpMtuOverride
			}
			if tcpMax < 512 || tcpMax > 65535 {
				fmt.Fprintf(os.Stderr, "-tcp-mtu %d out of range 512..65535\n", tcpMax)
				os.Exit(1)
			}
			dnsConn, err := NewTCPServerPacketConn(tcpAddr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "opening TCP listener: %v\n", err)
				os.Exit(1)
			}
			dnsConns = append(dnsConns, dnsConn)
			dnsCaps = append(dnsCaps, tcpMax)
			if len(dnsConns) == 2 && dnsCaps[0] != dnsCaps[1] {
				log.Printf("dual listeners: UDP cap %d, TCP cap %d", dnsCaps[0], dnsCaps[1])
			}
		}

		// One run() per listener. Each sets up its own turbotunnel/KCP
		// session space; a client uses exactly one transport, so sessions
		// never span listeners. run() now returns non-nil when its loops
		// die, so the first error kills the process (supervised restart)
		// instead of running deaf on one transport.
		errCh := make(chan error, len(dnsConns))
		for i, dc := range dnsConns {
			go func(dc net.PacketConn, cap int) {
				errCh <- run(privkey, domain, upstream, dc, cap)
			}(dc, dnsCaps[i])
		}
		if err := <-errCh; err != nil {
			log.Fatal(err)
		}
	}
}
