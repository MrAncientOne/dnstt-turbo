package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"www.bamsoftware.com/git/dnstt.git/bbr"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

const (
	// How many bytes of random padding to insert into queries.
	numPadding = 3
	// In an otherwise empty polling query, insert even more random padding,
	// to reduce the chance of a cache hit. Cannot be greater than 31,
	// because the prefix codes indicating padding start at 224.
	numPaddingForPoll = 8

	pollDelayMultiplier = 2.0
)

// Tuning knobs, defaulted to the proven values and overridable by flags in
// main() (see -init-poll-delay, -max-poll-delay, -poll-limit, -poll-burst).
// They are package vars, not consts, so flags can set them before the send/recv
// loops start. Do not mutate them after NewDNSPacketConn is called.
var (
	// The poll timer sends an empty polling query after this long without a
	// send; it backs off by pollDelayMultiplier up to maxPollDelay, and
	// resets to initPollDelay on any real send.
	initPollDelay = 100 * time.Millisecond
	maxPollDelay  = 10 * time.Second

	// pollLimit is the baseline in-flight poll depth. With pollBurst polls
	// enqueued per data-carrying response, in-flight grows toward the live
	// target (see pollTarget), then the KCP receive window becomes the
	// binding limit on download data in flight. Without -bbr the target
	// stays at pollLimit; keep it a little above
	// (window*KCP-MTU)/response-size so a lost/late response can't starve it.
	pollLimit = 24

	// pollBurst is how many polls are enqueued per data-carrying response.
	// 1 only maintains the in-flight window (ping-pong, ~2 responses => low
	// download); >=2 grows it to fill the pipe. This was THE download lever.
	pollBurst = 2
)

// pollChanCap is the permit buffer ceiling. It is deliberately generous:
// the live depth is governed by pollTarget (nominal) and the KCP window
// (binding), so a deep pipe never wedges on a full permit buffer while a
// shallow one enqueues no more than its target.
const pollChanCap = 8192

// pollTarget is the live in-flight poll target. Static pollLimit seeds it
// (manual path, and the floor the estimator never goes below); with -bbr
// the estimator ticker raises it to fit measured BDP (see SetPollTarget),
// so any link speed gets a deep enough download pipeline with no knob.
var pollTarget atomic.Int64

// min/maxPollTarget bound the adaptive depth: below 8 a single loss
// starves the pipe; the upper bound limits outstanding polling work.
const (
	minPollTarget = 8
	maxPollTarget = 4096
)

// SetPollTarget moves the live poll depth, clamped to [8,4096]. Values at
// or below pollLimit are raised to it: the estimator only ever deepens the
// pipeline, never thins it below the proven static baseline.
func SetPollTarget(n int) {
	if n < pollLimit {
		n = pollLimit
	}
	if n < minPollTarget {
		n = minPollTarget
	}
	if n > maxPollTarget {
		n = maxPollTarget
	}
	pollTarget.Store(int64(n))
}

// PollTarget reports the live poll depth (flags/tests/observability).
func PollTarget() int {
	if v := int(pollTarget.Load()); v > 0 {
		return v
	}
	return pollLimit
}

// PollDepthForBDP converts a BDP estimate into a poll depth for responses
// of avgRespBytes: how many responses must be in flight to hold one BDP,
// plus headroom for a loss. Pure function so the sizing is unit-tested.
// avgRespBytes <= 0 means unmeasured: hold the static baseline.
func PollDepthForBDP(bdpBytes uint64, avgRespBytes int) int {
	if avgRespBytes <= 0 {
		return pollLimit
	}
	d := int(bdpBytes/uint64(avgRespBytes)) + 4
	if d < pollLimit {
		d = pollLimit
	}
	if d > maxPollTarget {
		d = maxPollTarget
	}
	return d
}

// base32Encoding is a base32 encoding without padding.
var base32Encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// ednsDataOptionCode is a private-use EDNS(0) option code. In -tcp mode the
// client carries the upstream data packet in an OPT option with this code
// instead of in the QNAME, lifting the per-query upload cap from ~132 B (DNS
// name limit) to the KCP MTU. The server reads it back the same way. Verified
// to traverse the port-53 middlebox (dns53_upload_probe).
const ednsDataOptionCode = 0xfde9

// ednsUploadMTU is the upstream KCP MTU used in -tcp mode, i.e. the max bytes of
// application data per query carried in the EDNS option. 1400 lifts upload ~10x
// over the QNAME-bound ~132 B without making any single query unusually large.
const ednsUploadMTU = 1400

// DNSPacketConn provides a packet-sending and -receiving interface over various
// forms of DNS. It handles the details of how packets and padding are encoded
// as a DNS name in the Question section of an upstream query, and as a TXT RR
// in downstream responses.
//
// DNSPacketConn does not handle the mechanics of actually sending and receiving
// encoded DNS messages. That is rather the responsibility of some other
// net.PacketConn such as net.UDPConn, HTTPPacketConn, or TLSPacketConn, one of
// which must be provided to NewDNSPacketConn.
//
// We don't have a need to match up a query and a response by ID. Queries and
// responses are vehicles for carrying data and for our purposes don't need to
// be correlated. When sending a query, we generate a random ID, and when
// receiving a response, we ignore the ID.
type DNSPacketConn struct {
	transport net.PacketConn
	closeOnce sync.Once
	workers   sync.WaitGroup
	clientID  turbotunnel.ClientID
	domain    dns.Name
	// edns0PayloadClass is the requester's advertised UDP payload size, put
	// in the OPT RR of every query. The server refuses to send responses
	// larger than this. Over UDP keep it modest (fragmentation); over TCP
	// set it large so a big server -mtu is allowed.
	edns0PayloadClass uint16
	// uploadInEDNS carries the upstream data packet in an EDNS option rather
	// than the QNAME (raises the upload per-query cap). Enabled for -tcp.
	uploadInEDNS bool
	// Sending on pollChan permits sendLoop to send an empty polling query.
	// sendLoop also does its own polling according to a time schedule.
	pollChan chan struct{}
	// QueuePacketConn is the direct receiver of ReadFrom and WriteTo calls.
	// recvLoop and sendLoop take the messages out of the receive and send
	// queues and actually put them on the network.
	*turbotunnel.QueuePacketConn
	// pacer, when non-nil (-bbr), smooths query emission at the estimated
	// bottleneck rate instead of bursting on arrival.
	pacer       *bbr.Pacer
	feedback    *bbr.Feedback
	controlChan chan []byte
	// mon feeds the BBR-lite estimator (-bbr). Cheap counters, always on.
	mon *bbrMonitor
}

// bbrMonitor gathers delivery-rate and RTT samples for one BBR-lite
// controller tick. All methods are mutex-guarded; ticks run at ~10 Hz.
//
// RTT uses exact query/response matching by DNS message ID (RFC 7766
// section 7: pipelined responses can arrive out of order, so clients MUST
// match on Message ID). Each send records its ID with an emission
// timestamp; each response looks up its own ID. A match measures that
// query's own turnaround, never another query's queue position, so deep
// pipelines and floods cannot inflate the estimate (live failure it
// replaces: FIFO oldest-send matching reported 10s on an 84ms path,
// exploding BDP and pinning the window at max).
//
// IDs are drawn collision-free against outstanding ones (RFC 7766: MUST
// NOT reuse an in-flight ID); residual duplicates from the 16-bit space
// pair oldest-with-oldest inside per-ID FIFOs, which stays correct.
type bbrMonitor struct {
	mu        sync.Mutex
	delivered uint64 // downstream payload bytes received since last tick
	sentBytes uint64 // upstream wire bytes sent since last tick
	// respBytes/dataResps accumulate the mean data-response size over the
	// run (server -mtu is fixed per run, so a cumulative mean is the
	// stable quantity the poll-depth sizer needs, not a tick sample).
	respBytes uint64
	dataResps uint64
	// pending maps query ID to send timestamps (FIFO) of queries without
	// a response yet. Bounded by pendingCount; entries older than
	// monitorEntryTTL are swept each tick so a TCP drop can never grow
	// memory or wedge the idle detector.
	pending      map[uint16][]time.Time
	pendingCount int
	// Only empty polls consume the download polling budget. Upload and
	// ACK-carrying queries have independent lifetimes and must not starve it.
	pendingPolls map[uint16]time.Time
	// Lower bound on the oldest live poll timestamp. Removed polls may
	// leave an older bound; that only causes an extra sweep, never a late
	// expiry. This avoids a full map scan for every query/response.
	oldestPoll time.Time
	lastRTT    time.Duration
	// order records IDs in send order for oldest-first overflow drops;
	// consumed from orderHead with lazy skipping of already-matched IDs.
	order     []uint16
	orderHead int
	// rttMin is the minimum matched RTT observed since last tick.
	rttMin time.Duration
	hasRTT bool
	// tickStart bounds the current Sample interval.
	tickStart time.Time
	// carryTicks counts consecutive stalled ticks folded into the current
	// interval (see tick). Bounds how long a stall can stretch one sample.
	carryTicks int
	// now supplies timestamps; production uses wall time, tests inject a
	// fake clock for determinism. Nil means wall time (zero value safe).
	now func() time.Time
}

// maxCarryTicks caps carry-over at ~1s (10 ticks): a stall longer than
// that emits one zero-rate stalled sample per second instead of ten per
// second, so the estimator's window fills with stall evidence 10x slower
// but still converges on true collapse within ~8-16s.
const maxCarryTicks = 10

// maxMonitorInflight caps outstanding query timings at the DNS ID space
// minus one, supporting adaptive pipelines while bounding stalled history.
const maxMonitorInflight = 65535

// monitorEntryTTL bounds how long an unmatched send is trusted. Past it
// the query is presumed lost with its TCP connection (server drops
// responses on reconnect; KCP retransmits and the client re-polls).
const monitorEntryTTL = 30 * time.Second

// maxIDReserveTries bounds collision-avoidance draws when reserving a
// query ID against outstanding ones.
const maxIDReserveTries = 8

func newBBRMonitor() *bbrMonitor {
	return &bbrMonitor{pending: make(map[uint16][]time.Time), pendingPolls: make(map[uint16]time.Time), tickStart: time.Now(), now: time.Now}
}

// timeNow is time.Now unless tests injected a fake clock.
func (m *bbrMonitor) timeNow() time.Time {
	if m.now == nil {
		return time.Now()
	}
	return m.now()
}

// reserveID draws a query ID with no outstanding use (RFC 7766: MUST NOT
// reuse an in-flight ID). After bounded tries it returns a random ID
// anyway; per-ID FIFOs keep any residual duplicate pairing correct.
// Only sendLoop calls this, back-to-back with sent, so the reservation
// cannot be stolen between the two calls.
func (m *bbrMonitor) reserveID() uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var id uint16
	for i := 0; i < maxIDReserveTries; i++ {
		binary.Read(rand.Reader, binary.BigEndian, &id)
		if len(m.pending[id]) == 0 {
			return id
		}
	}
	// A crowded ID space still has a free slot because the timing table
	// is capped below 65536. Do not knowingly reuse an outstanding ID.
	for i := 0; i < 65536; i++ {
		if len(m.pending[id]) == 0 {
			return id
		}
		id++
	}
	return id
}

// sent records one emitted query of wireBytes wire bytes under id.
func (m *bbrMonitor) sent(wireBytes int, id uint16) {
	m.sentQuery(wireBytes, id, false)
}

func (m *bbrMonitor) sentQuery(wireBytes int, id uint16, poll bool) time.Time {
	now := m.timeNow()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sentBytes += uint64(wireBytes)
	m.pending[id] = append(m.pending[id], now)
	m.order = append(m.order, id)
	m.pendingCount++
	if poll {
		if m.pendingPolls == nil {
			m.pendingPolls = make(map[uint16]time.Time)
		}
		m.pendingPolls[id] = now
		if m.oldestPoll.IsZero() || now.Before(m.oldestPoll) {
			m.oldestPoll = now
		}
	}
	for m.pendingCount > maxMonitorInflight {
		// Stall: responses stopped but polls continue. Drop the oldest
		// unmatched send so memory stays bounded; the table stays
		// non-empty so ticks correctly report stalled (not idle).
		if m.orderHead >= len(m.order) {
			break
		}
		old := m.order[m.orderHead]
		m.orderHead++
		if q := m.pending[old]; len(q) > 0 {
			delete(m.pendingPolls, old)
			if len(q) == 1 {
				delete(m.pending, old)
			} else {
				m.pending[old] = q[1:]
			}
			m.pendingCount--
		}
	}
	if m.orderHead > 4096 && m.orderHead > len(m.order)/2 {
		m.order = append([]uint16(nil), m.order[m.orderHead:]...)
		m.orderHead = 0
	}
	return now
}

// cancelQuery rolls back registration on a failed write. A fast response may
// already have removed it, in which case there is nothing left to cancel.
func (m *bbrMonitor) cancelQuery(id uint16, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.pending[id]
	for i, t := range q {
		if t.Equal(at) {
			q = append(q[:i], q[i+1:]...)
			m.pendingCount--
			if len(q) == 0 {
				delete(m.pending, id)
			} else {
				m.pending[id] = q
			}
			if m.pendingPolls[id].Equal(at) {
				delete(m.pendingPolls, id)
			}
			break
		}
	}
}

func (m *bbrMonitor) PollInflight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pendingPolls) == 0 {
		m.oldestPoll = time.Time{}
		return 0
	}
	// Replace orphaned polls promptly after a carrier interruption while
	// retaining their IDs for late replies and RTT pairing.
	ttl := 4 * m.lastRTT
	if ttl < time.Second {
		ttl = time.Second
	}
	if ttl > 10*time.Second {
		ttl = 10 * time.Second
	}
	cutoff := m.timeNow().Add(-ttl)
	if !m.oldestPoll.IsZero() && m.oldestPoll.After(cutoff) {
		return len(m.pendingPolls)
	}
	m.oldestPoll = time.Time{}
	for id, at := range m.pendingPolls {
		if !at.After(cutoff) {
			delete(m.pendingPolls, id)
		} else if m.oldestPoll.IsZero() || at.Before(m.oldestPoll) {
			m.oldestPoll = at
		}
	}
	return len(m.pendingPolls)
}

// receivedByID records one response carrying payloadBytes of downstream
// data for query id. An exact ID hit differences that query's own send
// time: a true turnaround sample admitted to the tick minimum within
// physical bounds. Unknown IDs (swept as stale, or answers to pre-drop
// queries) still count bytes but yield no sample.
func (m *bbrMonitor) receivedByID(payloadBytes int, id uint16) {
	now := m.timeNow()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delivered += uint64(payloadBytes)
	if payloadBytes > 0 {
		m.respBytes += uint64(payloadBytes)
		m.dataResps++
	}
	q := m.pending[id]
	if len(q) == 0 {
		return
	}
	sentAt := q[0]
	if m.pendingPolls[id].Equal(sentAt) {
		delete(m.pendingPolls, id)
	}
	if len(q) == 1 {
		delete(m.pending, id)
	} else {
		m.pending[id] = q[1:]
	}
	m.pendingCount--
	if rtt := now.Sub(sentAt); rtt >= minRTTSampleFloor && rtt <= 30*time.Second {
		m.lastRTT = rtt
		if !m.hasRTT || rtt < m.rttMin {
			m.rttMin = rtt
			m.hasRTT = true
		}
	}
}

// minRTTSampleFloor is the smallest matched RTT admitted to a tick.
// Below this is loopback/scheduling coincidence, never a VPS path.
const minRTTSampleFloor = 10 * time.Millisecond

// Inflight reports queries sent without a response yet (the live
// download pipeline depth the poll gate budgets against).
func (m *bbrMonitor) Inflight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingCount
}

// AvgResponse reports the mean data-response size in bytes, or 0 while
// unmeasured (no data response seen yet).
func (m *bbrMonitor) AvgResponse() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dataResps == 0 {
		return 0
	}
	return int(m.respBytes / m.dataResps)
}

// tick snapshots one Sample and resets for the next interval, with one
// exception: a stalled tick (nothing delivered, queries outstanding) folds
// its interval into the next tick instead of emitting a zero-rate sample,
// up to maxCarryTicks. Fixed 100ms ticking would otherwise mint
// zero-rate points whenever a response lands just past a tick boundary or
// delivery arrives in clumps, and a window half full of such artifacts
// reads as collapse (live failure: sustained median zero while the app
// moved a megabyte per second). The delivery-rate literature computes per
// acknowledgment precisely to avoid this; carry-over is the ticked
// equivalent. A stall past the cap emits one zero-rate stalled sample per
// second, so true collapse still converges, only slower. Idle
// (application-limited) ticks always emit immediately: silence with
// nothing outstanding is information, not artifact. A carried (non-emitted)
// tick returns Interval 0, which Update skips, reporting only whether the
// path looks stalled versus idle. The TTL sweep runs on every tick
// including carried ones, so orphaned sends can never grow memory.
func (m *bbrMonitor) tick() bbr.Sample {
	now := m.timeNow()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if m.delivered == 0 && len(m.pending) > 0 && m.carryTicks < maxCarryTicks {
		m.carryTicks++
		return bbr.Sample{Interval: 0, AppLimited: false}
	}
	s := bbr.Sample{
		DeliveredBytes: m.delivered,
		SentBytes:      m.sentBytes,
		Interval:       now.Sub(m.tickStart),
		AppLimited:     m.delivered == 0 && m.pendingCount == 0,
		RTT:            m.rttMin,
		HasRTT:         m.hasRTT,
	}
	m.delivered = 0
	m.sentBytes = 0
	m.hasRTT = false
	m.carryTicks = 0
	m.tickStart = now
	return s
}

// Housekeeping is unconditional; BBR only controls estimator sampling.
func (m *bbrMonitor) prune() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(m.timeNow())
}

func (m *bbrMonitor) pruneLocked(now time.Time) {
	cutoff := now.Add(-monitorEntryTTL)
	for id, q := range m.pending {
		keep := q[:0]
		for _, at := range q {
			if at.After(cutoff) {
				keep = append(keep, at)
			} else {
				m.pendingCount--
				if m.pendingPolls[id].Equal(at) {
					delete(m.pendingPolls, id)
				}
			}
		}
		if len(keep) == 0 {
			delete(m.pending, id)
		} else {
			m.pending[id] = keep
		}
	}
	// Bound the send-order log. Every send appends one entry, but matches
	// only remove map entries: without this the log grows one entry per
	// query for the life of the process even when every query is answered
	// (entries were previously consumed only on the stall-overflow path).
	// Advance past fully-matched leading IDs, compact the prefix, and
	// hard-cap the retained tail oldest-first — exactly what the overflow
	// path would drop. Skipped entries are already accounted (their
	// timestamps left pendingCount at match/sweep time); hard-capped live
	// entries keep their timestamps, so counts stay exact.
	for m.orderHead < len(m.order) && len(m.pending[m.order[m.orderHead]]) == 0 {
		m.orderHead++
	}
	// Absolute cap, oldest-first (mirrors the overflow path): even IDs
	// with live sends leave the log past 4x the in-flight cap — their
	// timestamps stay in pending, only log order is shed.
	for len(m.order)-m.orderHead > 4*maxMonitorInflight {
		m.orderHead++
	}
	if (m.orderHead > 4096 && m.orderHead > len(m.order)/2) || len(m.order) > 8*maxMonitorInflight {
		m.order = append([]uint16(nil), m.order[m.orderHead:]...)
		m.orderHead = 0
	}
}

// NewDNSPacketConn creates a new DNSPacketConn. transport, through its WriteTo
// and ReadFrom methods, handles the actual sending and receiving the DNS
// messages encoded by DNSPacketConn. addr is the address to be passed to
// transport.WriteTo whenever a message needs to be sent.
func NewDNSPacketConn(transport net.PacketConn, addr net.Addr, domain dns.Name, edns0PayloadClass uint16, uploadInEDNS bool) *DNSPacketConn {
	// Generate a new random ClientID.
	clientID := turbotunnel.NewClientID()
	if edns0PayloadClass == 0 {
		edns0PayloadClass = 4096
	}
	c := &DNSPacketConn{
		transport:         transport,
		clientID:          clientID,
		domain:            domain,
		edns0PayloadClass: edns0PayloadClass,
		uploadInEDNS:      uploadInEDNS,
		pollChan:          make(chan struct{}, pollChanCap),
		QueuePacketConn:   turbotunnel.NewQueuePacketConn(clientID, 0),
		mon:               newBBRMonitor(),
	}
	if bbrOn {
		c.pacer = bbr.NewPacer(nil)
		c.feedback = bbr.NewFeedback(nil)
		c.controlChan = make(chan []byte, 512)
	}
	pollTarget.Store(int64(pollLimit))
	c.workers.Add(3)
	go func() {
		defer c.workers.Done()
		defer c.Close()
		err := c.recvLoop(transport)
		if err != nil {
			log.Printf("recvLoop: %v", err)
		}
	}()
	go func() {
		defer c.workers.Done()
		defer c.Close()
		err := c.sendLoop(transport, addr)
		if err != nil {
			log.Printf("sendLoop: %v", err)
		}
	}()
	go func() {
		defer c.workers.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.Done():
				return
			case <-ticker.C:
				c.mon.prune()
			}
		}
	}()
	return c
}

func (c *DNSPacketConn) Close() error {
	c.closeOnce.Do(func() {
		c.QueuePacketConn.Close()
		if c.transport != nil {
			c.transport.Close()
		}
	})
	return nil
}

// dnsResponsePayload extracts the downstream payload of a DNS response, encoded
// into the RDATA of a TXT RR. It returns nil if the message doesn't pass format
// checks, or if the name in its Question entry is not a subdomain of domain.
func dnsResponsePayload(resp *dns.Message, domain dns.Name) []byte {
	if resp.Flags&0x8000 != 0x8000 {
		// QR != 1, this is not a response.
		return nil
	}
	if resp.Flags&0x000f != dns.RcodeNoError {
		return nil
	}

	if len(resp.Answer) != 1 {
		return nil
	}
	answer := resp.Answer[0]

	_, ok := answer.Name.TrimSuffix(domain)
	if !ok {
		// Not the name we are expecting.
		return nil
	}

	if answer.Type != dns.RRTypeTXT {
		// We only support TYPE == TXT.
		return nil
	}
	payload, err := dns.DecodeRDataTXT(answer.Data)
	if err != nil {
		return nil
	}

	return payload
}

// nextPacket reads the next length-prefixed packet from r. It returns a nil
// error only when a complete packet was read. It returns io.EOF only when there
// were 0 bytes remaining to read from r. It returns io.ErrUnexpectedEOF when
// EOF occurs in the middle of an encoded packet.
func nextPacket(r *bytes.Reader) ([]byte, error) {
	for {
		var n uint16
		err := binary.Read(r, binary.BigEndian, &n)
		if err != nil {
			// We may return a real io.EOF only here.
			return nil, err
		}
		p := make([]byte, n)
		_, err = io.ReadFull(r, p)
		// Here we must change io.EOF to io.ErrUnexpectedEOF.
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return p, err
	}
}

// recvLoop repeatedly calls transport.ReadFrom to receive a DNS message,
// extracts its payload and breaks it into packets, and stores the packets in a
// queue to be returned from a future call to c.ReadFrom.
//
// Whenever we receive a DNS response containing at least one data packet, we
// send on c.pollChan to permit sendLoop to send an immediate polling queries.
// KCP itself will also send an ACK packet for incoming data, which is
// effectively a second poll. Therefore, each time we receive data, we send up
// to 2 polling queries (or 1 + f polling queries, if KCP only ACKs an f
// fraction of incoming data). We say "up to" because sendLoop will discard an
// empty polling query if it has an organic non-empty packet to send (this goes
// also for KCP's organic ACK packets).
//
// The intuition behind polling immediately after receiving is that if server
// has just had something to send, it may have more to send, and in order for
// the server to send anything, we must give it a query to respond to. The
// intuition behind polling *2 times* (or 1 + f times) is similar to TCP slow
// start: we want to maintain some number of queries "in flight", and the faster
// the server is sending, the higher that number should be. If we polled only
// once for each received packet, we would tend to have only one query in flight
// at a time, ping-pong style. The first polling query replaces the in-flight
// query that has just finished its duty in returning data to us; the second
// grows the effective in-flight window proportional to the rate at which
// data-carrying responses are being received. Compare to Eq. (2) of
// https://tools.ietf.org/html/rfc5681#section-3.1. The differences are that we
// count messages, not bytes, and we don't maintain an explicit window. If a
// response comes back without data, or if a query or response is dropped by the
// network, then we don't poll again, which decreases the effective in-flight
// window.
func (c *DNSPacketConn) recvLoop(transport net.PacketConn) error {
	// 64 KiB so large DNS-over-TCP responses (server -mtu up to the DNS/TCP
	// ceiling) are not truncated. Over UDP the transport still returns small
	// datagrams, so this only raises the cap. Extracted packets are fresh
	// copies, so the buffer is safe to reuse.
	var buf [65536]byte
	for {
		n, addr, err := transport.ReadFrom(buf[:])
		if err != nil {
			return err
		}

		// Got a response. Try to parse it as a DNS message.
		resp, err := dns.MessageFromWireFormat(buf[:n])
		if err != nil {
			health.Failure("dns_parse_failed", err)
			continue
		}

		if code := resp.Rcode(); code != dns.RcodeNoError {
			health.Failure("dns_rcode_error", fmt.Errorf("rcode=%d", code))
		}
		payload := dnsResponsePayload(&resp, c.domain)

		// Pull out the packets contained in the payload.
		r := bytes.NewReader(payload)
		any := false
		nbytes := 0
		unique := 0
		for {
			p, err := nextPacket(r)
			if err != nil {
				break
			}
			any = true
			nbytes += len(p)
			if c.feedback != nil {
				unique += c.feedback.Received(p)
			}
			c.QueuePacketConn.QueueIncoming(p, addr)
		}
		if c.feedback != nil {
			c.feedback.RecordResponse(unique)
		}
		c.mon.receivedByID(nbytes, resp.ID)

		// If the payload contained one or more packets, permit sendLoop
		// to poll immediately. We enqueue TWO polls per data-carrying
		// response: one replaces the in-flight query that just returned
		// data, the second grows the in-flight window (TCP-slow-start
		// style) so the download pipe actually fills. Upstream dnstt
		// relied on KCP's ACK-polls for that second poll, but over the
		// reliable, in-order DNS-over-TCP substrate KCP ACKs are too
		// sparse to grow the window, leaving download stuck near
		// ping-pong (~2 responses in flight). In-flight is budgeted against
		// the live pollTarget (static baseline, BBR-raised to fit BDP), and
		// the KCP receive window stays the binding limit on download data,
		// so this cannot run away on any link speed.
		if any {
			for i := 0; i < pollBurst && c.mon.PollInflight() < PollTarget(); i++ {
				select {
				case c.pollChan <- struct{}{}:
				default:
				}
			}
		}
	}
}

// chunks breaks p into non-empty subslices of at most n bytes, greedily so that
// only final subslice has length < n.
func chunks(p []byte, n int) [][]byte {
	var result [][]byte
	for len(p) > 0 {
		sz := len(p)
		if sz > n {
			sz = n
		}
		result = append(result, p[:sz])
		p = p[sz:]
	}
	return result
}

// send sends p as a single packet encoded into a DNS query, using
// transport.WriteTo(query, addr). The length of p must be less than 224 bytes.
//
// Here is an example of how a packet is encoded into a DNS name, using
//
//	p = "supercalifragilisticexpialidocious"
//	c.clientID = "CLIENTID"
//	domain = "t.example.com"
//
// as the input.
//
//  0. Start with the raw packet contents.
//
//     supercalifragilisticexpialidocious
//
//  1. Length-prefix the packet and add random padding. A length prefix L < 0xe0
//     means a data packet of L bytes. A length prefix L ≥ 0xe0 means padding
//     of L − 0xe0 bytes (not counting the length of the length prefix itself).
//
//     \xe3\xd9\xa3\x15\x22supercalifragilisticexpialidocious
//
//  2. Prefix the ClientID.
//
//     CLIENTID\xe3\xd9\xa3\x15\x22supercalifragilisticexpialidocious
//
//  3. Base32-encode, without padding and in lower case.
//
//     ingesrkokreujy6zumkse43vobsxey3bnruwm4tbm5uwy2ltoruwgzlyobuwc3djmrxwg2lpovzq
//
//  4. Break into labels of at most 63 octets.
//
//     ingesrkokreujy6zumkse43vobsxey3bnruwm4tbm5uwy2ltoruwgzlyobuwc3d.jmrxwg2lpovzq
//
//  5. Append the domain.
//
//     ingesrkokreujy6zumkse43vobsxey3bnruwm4tbm5uwy2ltoruwgzlyobuwc3d.jmrxwg2lpovzq.t.example.com
func (c *DNSPacketConn) send(transport net.PacketConn, p []byte, addr net.Addr) error {
	// Charge PUSH payload (including retransmissions) in the same units as
	// delivery feedback. Service reverse ACKs and polls while bulk waits.
	if c.needsPacing(p) {
		if delay := c.pacer.Advance(bbr.PayloadBytes(p)); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
		waiting:
			for {
				select {
				case <-c.Done():
					return net.ErrClosed
				case <-timer.C:
					break waiting
				case control := <-c.controlChan:
					if err := c.send(transport, control, addr); err != nil {
						return err
					}
				case <-c.pollChan:
					if c.mon.PollInflight() < PollTarget() {
						if err := c.send(transport, nil, addr); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	var decoded []byte
	var optData []byte // EDNS OPT rdata (our data option, when uploadInEDNS)
	if c.uploadInEDNS {
		// QNAME carries only the ClientID + cache-busting padding; the
		// data packet rides in an EDNS option, so it is not bound by the
		// ~132 B DNS-name limit.
		var buf bytes.Buffer
		buf.Write(c.clientID[:])
		n := numPaddingForPoll
		buf.WriteByte(byte(224 + n))
		io.CopyN(&buf, rand.Reader, int64(n))
		decoded = buf.Bytes()
		if len(p) > 0 {
			// One EDNS option: code(2) + length(2) + packet.
			optData = make([]byte, 4+len(p))
			binary.BigEndian.PutUint16(optData[0:2], ednsDataOptionCode)
			binary.BigEndian.PutUint16(optData[2:4], uint16(len(p)))
			copy(optData[4:], p)
		}
	} else {
		if len(p) >= 224 {
			return fmt.Errorf("too long")
		}
		var buf bytes.Buffer
		// ClientID
		buf.Write(c.clientID[:])
		n := numPadding
		if len(p) == 0 {
			n = numPaddingForPoll
		}
		// Padding / cache inhibition
		buf.WriteByte(byte(224 + n))
		io.CopyN(&buf, rand.Reader, int64(n))
		// Packet contents
		if len(p) > 0 {
			buf.WriteByte(byte(len(p)))
			buf.Write(p)
		}
		decoded = buf.Bytes()
	}

	encoded := make([]byte, base32Encoding.EncodedLen(len(decoded)))
	base32Encoding.Encode(encoded, decoded)
	encoded = bytes.ToLower(encoded)
	labels := chunks(encoded, 63)
	labels = append(labels, c.domain...)
	name, err := dns.NewName(labels)
	if err != nil {
		return err
	}

	var id = c.mon.reserveID()
	query := &dns.Message{
		ID:    id,
		Flags: 0x0100, // QR = 0, RD = 1
		Question: []dns.Question{
			{
				Name:  name,
				Type:  dns.RRTypeTXT,
				Class: dns.ClassIN,
			},
		},
		// EDNS(0); Data carries our upstream packet option in -tcp mode.
		Additional: []dns.RR{
			{
				Name:  dns.Name{},
				Type:  dns.RRTypeOPT,
				Class: c.edns0PayloadClass, // requester's UDP payload size
				TTL:   0,                   // extended RCODE and flags
				Data:  optData,
			},
		},
	}
	buf, err := query.WireFormat()
	if err != nil {
		return err
	}

	if c.feedback != nil {
		c.feedback.Sent(p)
	}

	// Register before emission: recvLoop can see the reply before the
	// transport write returns. Failed writes must release that registration.
	at := c.mon.sentQuery(len(buf), id, len(p) == 0)
	_, err = transport.WriteTo(buf, addr)
	if err != nil {
		c.mon.cancelQuery(id, at)
	}
	return err
}

// needsPacing reports whether a data packet must wait behind the pacer:
// only bulk upstream payload with an active pacer. Extracted (not inlined)
// so the bypass contract — polls and the ACK/control plane never wait —
// is unit-pinned, since getting it wrong wedges the tunnel.
func (c *DNSPacketConn) needsPacing(p []byte) bool {
	return c.pacer != nil && bbr.PayloadBytes(p) > 0
}

// WriteTo gives control-only KCP frames a separate bounded queue so a
// paced upload cannot withhold the ACKs needed by a simultaneous download.
func (c *DNSPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.feedback != nil {
		c.feedback.Queued(p)
	}
	if c.controlChan == nil || bbr.PayloadBytes(p) > 0 {
		n, accepted, err := c.QueuePacketConn.TryWriteTo(p, addr)
		if !accepted && c.feedback != nil {
			c.feedback.Unqueued(p)
		}
		return n, err
	}
	copyP := append([]byte(nil), p...)
	select {
	case <-c.Done():
		return 0, net.ErrClosed
	case c.controlChan <- copyP:
	default: // KCP repeats ACKs and carries cumulative UNA on later frames.
	}
	return len(p), nil
}

// sendLoop takes packets that have been written using c.WriteTo, and sends them
// on the network using send. It also does polling with empty packets when
// requested by pollChan or after a timeout.
func (c *DNSPacketConn) sendLoop(transport net.PacketConn, addr net.Addr) error {
	pollDelay := initPollDelay
	pollTimer := time.NewTimer(pollDelay)
	defer pollTimer.Stop()
	for {
		select {
		case <-c.Done():
			return net.ErrClosed
		default:
		}
		select {
		case control := <-c.controlChan:
			if err := c.send(transport, control, addr); err != nil {
				health.Failure("dns_control_send_failed", err)
			}
			continue
		default:
		}
		var p []byte
		outgoing := c.QueuePacketConn.OutgoingQueue(addr)
		pollTimerExpired := false
		// Prioritize sending an actual data packet from outgoing. Only
		// consider a poll when outgoing is empty.
		select {
		case p = <-outgoing:
		default:
			select {
			case <-c.Done():
				return net.ErrClosed
			case p = <-c.controlChan:
			case p = <-outgoing:
			case <-c.pollChan:
			case <-pollTimer.C:
				pollTimerExpired = true
			}
		}

		if len(p) > 0 {
			// A data-carrying packet displaces one pending poll
			// opportunity, if any.
			select {
			case <-c.pollChan:
			default:
			}
		}

		if pollTimerExpired {
			// We're polling because it's been a while since we last
			// polled. Increase the poll delay.
			pollDelay = time.Duration(float64(pollDelay) * pollDelayMultiplier)
			if pollDelay > maxPollDelay {
				pollDelay = maxPollDelay
			}
		} else {
			// We're sending an actual data packet, or we're polling
			// in response to a received packet. Reset the poll
			// delay to initial.
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollDelay = initPollDelay
		}
		pollTimer.Reset(pollDelay)

		// Unlike in the server, in the client we assume that because
		// the data capacity of queries is so limited, it's not worth
		// trying to send more than one packet per query.
		if len(p) == 0 && !pollTimerExpired && c.mon.PollInflight() >= PollTarget() {
			continue
		}
		err := c.send(transport, p, addr)
		if err != nil {
			health.Failure("dns_send_failed", err)
			continue
		}
	}
}
