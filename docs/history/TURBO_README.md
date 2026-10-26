# dnstt-turbo — a performance-tuned dnstt fork

A maintained fork of [dnstt](https://www.bamsoftware.com/software/dnstt/) (David
Fifield, MIT/0BSD) focused on **maximum throughput** through a DNS tunnel, with
configurable performance controls and a new
plain **DNS-over-TCP** transport for paths where UDP:53 is throttled but TCP:53
is not.

## What this fork adds over upstream dnstt

1. **`-tcp` transport (client + server): plain DNS-over-TCP on :53**, straight to
   the authoritative tunnel server — no DoH/DoT/TLS, no recursive resolver.
   Reaches the port-53 channel that some middleboxes throttle hard for UDP but
   pass over TCP with large responses. (Upstream dnstt is `-doh`/`-dot`/`-udp`
   only.) The server can serve `-udp` and `-tcp` at once.
2. **EDNS upstream carrier (`-tcp-upstream-mtu N`)**: carry upload data in an
   EDNS(0) option instead of the ~132-byte QNAME, lifting per-query upload
   ~4-10x. `0` keeps the robust QNAME encoding.
3. **Download window growth fix**: the client enqueues `-poll-burst` (default 2)
   polls per data response so the in-flight window actually fills the pipe —
   upstream dnstt leaned on KCP ACK-polls, which are too sparse over a reliable
   TCP substrate and left download near ping-pong (~2 responses in flight).
4. **All tuning is configurable** (KCP window, poll depth/burst/delays, response
   delay, KCP segment cap, smux buffer, response size), with optional independent
   upload/download adaptation and configurable flight budgets.
5. Zero-copy vectored framing on the TCP transport; the client recv buffer and
   server response bundling handle large (up to ~64 KB) DNS-over-TCP responses.

The Go module path is still `www.bamsoftware.com/git/dnstt.git` (internal only —
does not affect use). Rename it in `go.mod` + imports if you want your own path.

## Build
```bash
cd dnstt-turbo
( cd dnstt-server && go build )                         # native
( cd dnstt-client && go build )
# cross-compile, e.g. ARM64 server / AMD64 client:
( cd dnstt-server && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" )
( cd dnstt-client && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" )
```

## Speed recipes

### A. Adaptive direct DNS-over-TCP:53 (needs :53 reachable to your VPS)
Build both ends from the current source. This enables adaptive control; actual
throughput depends on the path, resolver/carrier limits, RTT and CPU:
```bash
# server (VPS): big responses = the download lever
dnstt-server -tcp :53 -mtu 16384 -privkey-file server.key DOMAIN 127.0.0.1:PORT
# client: EDNS upload + independent adaptive windows
dnstt-client -tcp VPS_IP:53 -tcp-upstream-mtu 500 -bbr -fair-upload \
             -pubkey HEX DOMAIN 127.0.0.1:7000
```
- `-mtu 16384` (server): 16 KB responses — the biggest single download lever.
- To serve both transports at once: `dnstt-server -udp :53 -tcp :53 -mtu 1232 -tcp-mtu 16384 ...` (UDP stays unfragmented, TCP uses big responses). A smaller advertised size must fit a complete configured KCP segment; otherwise it receives FORMERR. A bare `-mtu 16384` cannot serve 1232-byte clients safely.
- `-tcp-upstream-mtu 500`: upload size that survives the middlebox (find yours;
  see "Finding the max-safe upload"). `0` = safe QNAME-only (~0.1-0.3 Mbps up).
- `-bbr`: size upload and download windows independently from delivery and RTT.
  `-fair-upload` shares upload service across active application streams.

### B. Direct UDP:53 (if UDP:53 is clean and not throttled)
```bash
dnstt-server -udp :53 -mtu 1232 -privkey-file server.key DOMAIN 127.0.0.1:PORT
dnstt-client -udp VPS_IP:53 -pubkey HEX DOMAIN 127.0.0.1:7000
```
Keep `-mtu` near 1232 for UDP (fragmentation); UDP can't use the big-response lever.

### C. Public resolver (stealth / when direct :53 to your VPS is blocked)
Your DOMAIN must be NS-delegated to the server. Traffic rides real recursive
resolvers, so it's slower (resolver rate limits, ~small responses) but blends in:
```bash
# via DoH:
dnstt-client -doh https://cloudflare-dns.com/dns-query -pubkey HEX DOMAIN 127.0.0.1:7000
# via DoT:
dnstt-client -dot 1.1.1.1:853 -pubkey HEX DOMAIN 127.0.0.1:7000
# via a plain UDP resolver:
dnstt-client -udp 8.8.8.8:53  -pubkey HEX DOMAIN 127.0.0.1:7000
```
Tuning still applies, but throughput is bounded by the resolver, not your flags.

## Tuning flag reference

### Server
| flag | default | what it does |
|---|---|---|
| `-mtu N` | 1232 | max DNS **response** size per listener. Big = fewer round-trips = higher download. Use ~16384 for TCP; keep ~1232 for UDP. Responses also obey the advertised cap. Smaller caps are accepted only if they can carry the listener's whole KCP segment; unsupported sizes receive FORMERR before payload admission. |
| `-tcp-mtu N` | 0 (= `-mtu`) | with `-tcp`: response cap for the TCP listener only. E.g. `-udp :53 -tcp :53 -mtu 1232 -tcp-mtu 16384` keeps UDP unfragmented while TCP uses the big-response lever. |
| `-kcp-window N` | 65535 | Server send/receive ceiling in segments (1..65535); the client advertised window limits downstream flight. Static clients should tune their own window. |
| `-kcp-mtu-cap N` | 1400 | max KCP segment size. Large responses come from *bundling* many segments; KCP rejects huge MTUs, so this stays modest. |
| `-max-response-delay D` | 100ms | how long to hold a query for downstream data before answering empty. ~1 RTT. Lower = snappier idle; too low wastes queries. |
| `-stream-buffer N` | 16777216 | Server smux per-stream buffer. |
| `-fair-download` | off | fair-queue the **download** path: pace flows into smux in sparse-priority round-robin so a bulk flood (BitTorrent) can't starve interactive/DNS streams on the shared session. See below. |
| `-fair-chunk N` | 4096 | with `-fair-download`: max bytes served per flow per turn. |
| `-fair-idle D` | 100ms | with `-fair-download`: idle gap that (re)marks a flow *sparse* (interactive → priority). |
| `-fair-write-timeout D` | 500ms | with `-fair-download`: per-chunk smux write deadline; a stalled/slow-client flow yields instead of head-of-line-blocking the writer. Proven live: 50ms throttled bulk to death under a saturated window, 500ms flows full speed. |
| `-fair-max-burst N` | 8 | with `-fair-download`: consecutive sparse turns before one bulk turn is forced. Stops a storm of fresh connections starving bulk (the torrent jitter fix); `<=0` = strict sparse. |
| `-fair-max-sparse N` | 64 | with `-fair-download`: cap on the sparse list; overflow demotes to round-robin instead of growing priority without bound. |
| `-fair-submit-timeout D` | 0 | Wait through congestion with bounded queue backpressure. A positive value opts into stream failure when scheduler starvation exceeds this duration; use 0 for flaky uplinks. Applies to upload and download fairness. |
| `-fair-linger D` | 250ms | with `-fair-download`: after clean pump end, wait for quiet before FIN so it can't jump queued tail data in smux's priority send queue (`0` = off). |
| `-tcp-sockbuf N` | 0 | with `-tcp`: kernel socket buffers on the port-53 connection (`0` = OS default; e.g. 4194304 to absorb bursts). |
| `-smux-recvbuf N` | 33554432 | smux session-wide receive buffer (must stay ≥ `-stream-buffer`). |
| `-kcp-acknodelay` | off | flush KCP ACKs immediately instead of batching (lower latency, more upstream query traffic). |

### Client
| flag | default | what it does |
|---|---|---|
| `-fair-upload` | off | CAKE-schedule the **upload** path: sparse-priority + DRR++ deficit + Cobalt AQM in front of smux, so a bulk uploader (torrent/webtorrent, a seeding stream) can't starve interactive uploads — DNS, TLS/SOCKS handshakes, requests, piece requests. The mirror of `-fair-download`. With `-bbr`, slow paths retain admission shaping before smux; at high measured upload rates the scheduler bypasses that second clock, while carrier pacing remains active including retransmissions. Without `-bbr`, scheduling provides fairness without a rate ceiling. `-fair-chunk`/`-fair-idle`/`-fair-write-timeout` mean the same as on the server, as do `-fair-max-burst`/`-fair-max-sparse`/`-fair-submit-timeout` (storm protection) and `-fair-linger`. `-tcp-sockbuf`/`-smux-recvbuf`/`-kcp-acknodelay` exist on both ends. |

Run `-fair-download` on the server and `-fair-upload` on the client for fairness
in **both** directions with nothing to hand-tune. The CAKE scheduling components live
in-process (`cake/` + `fairqueue/`, reference source vendored at
`third_party/cake/`): this supersedes and retires the external
`cake-autorate.sh` + `BBR_RATES_FILE` path (the rates file is still published
for observability, but nothing consumes it anymore).

## Fair download scheduling (`-fair-download`)

The whole tunnel is one KCP/smux session. On download, each stream normally
copies from its upstream socket into smux greedily, so a bulk flood (a torrent
swarm = hundreds of streams) dominates the shared pipe and starves DNS/browsing.
The **client can only backpressure**; the **server is the sender**, so download
order can only be fixed here.

`-fair-download` interposes one writer per session in front of smux. Per-flow
readers hand it one bounded chunk at a time (blocking until it's written — that
backpressure makes a de-prioritized bulk flow stop reading its socket, so the
slowdown propagates to the internet peer and nothing buffers in RAM). The writer
serves:

1. **Sparse flows first** — a flow idle longer than `-fair-idle` before it
   produced this chunk (DNS, SSH, page control bytes, API calls). This is
   fq_codel's new-flow priority: classified by *behaviour*, so it's immune to a
   bulk app spreading load across thousands of connections.
2. **Otherwise DRR++ deficit round-robin** across active flows
   (`sch_cake.c:cake_dequeue`): each flow's deficit is topped up by the flow
   quantum and debited by bytes served, so bulk-vs-bulk shares byte-fair
   (a 4KB-chunk torrent flow and a 1KB-chunk web flow converge near 1:1
   bytes, not 4:1 turns). Quantum, CoDel target/interval and the shaper
   clocks all derive from the shaper rate (`cake_set_rate`); the shaper
   itself is deficit-mode (no burst parameter).

A `-fair-write-timeout` keeps one slow client from stalling the single writer.
Three bounds keep a connection storm from wedging the scheduler itself:
`-fair-max-burst` (default 8) forces a bulk turn after that many consecutive
sparse turns, `-fair-max-sparse` (default 64) demotes sparse overflow to the
ring, and `-fair-submit-timeout` (default 250ms) fails an unserved chunk fast
so the queue can never grow without bound — a stall becomes a quick retry.
The scheduler owns one outstanding smux write per flow. A scheduler timeout
yields to other flows; the next turn waits for that same write instead of
resending bytes. This matters because smux can transmit an enqueued frame
even after its native write deadline returns an error. Active service is
excluded from the submission starvation timeout. `-fair-linger` (default
250ms) retains the existing quiet period before closing a completed stream.
Both directions share one implementation (`fairqueue/` on top
of the `cake/` mechanism port; kernel reference vendored read-only at
`third_party/cake/sch_cake.c`), covered by policy, teardown, churn
(2000 flows), fairness, sparse-latency, and integrity tests plus
per-traffic-class benchmarks (`go test ./fairqueue/ ./cake/`), including a
shaper-pacing test, an overlimit-victim test (longest bulk dropped first),
and a DRR byte-fairness test.
It does **not** raise the ceiling (the middlebox byte cap is fixed) — it decides
*who* gets the pipe. Bulk-vs-bulk still shares fairly; a speedtest during a flood
shares too (correct). The upload side uses BBR's carrier payload pacer, including probing and
retransmissions (`-bbr`), while `-fair-upload` retains flow fairness and bounded
queueing. Slow paths retain admission shaping; high measured rates bypass
the second clock with hysteresis. The server download scheduler has no
estimator and runs fairness-only (unlimited ceiling).
At the default 4 KiB fair chunk, the second clock is bypassed only after
non-app-limited delivery sustains at least 4.096 MB/s of upload payload;
it returns below 2.048 MB/s. This avoids sub-millisecond admission waits
on fast paths without removing the slow-path burst protection.
Default off, so the binary is a drop-in until you switch it on.

### Client
| flag | default | what it does |
|---|---|---|
| `-tcp ADDR` | – | plain DNS-over-TCP transport to ADDR (your server on :53). |
| `-tcp-upstream-mtu N` | 0 | upload bytes/query via EDNS (0 = QNAME ~132 B). Larger = faster upload, until the middlebox mangles it. |
| `-kcp-window N` | 128 | KCP send/recv window (segments). The main throughput vs. bufferbloat lever. Rule of thumb: ~2-3 × (bandwidth × RTT) ÷ segment size. |
| `-poll-burst N` | 2 | polls per received data response. `>=2` grows the download window; `1` = ping-pong (slow). |
| `-bbr` | off | Independent upload/download delivery models, payload pacing and adaptive windows. Use the matching server build. See adaptive control below. |
| `-bbr-min-window N` | 32 | Minimum adaptive window in segments. |
| `-bbr-max-window N` | 65535 | Maximum adaptive window; KCP's wire format cannot advertise more. |
| `-bbr-max-flight N` | 16777216 | Target flight budget in payload bytes per direction (up to 1 GiB). A budget ceiling takes precedence over the minimum window. |
| `-bbr-min-rate N` | 1024 | Minimum upload pacing rate in payload bytes/second. Lower for extremely slow links. |
| `-poll-limit N` | 24 | baseline in-flight polls. With `-bbr` grows automatically to cover bounded receive flight ÷ observed response size (capped at 4096); without it, keep a bit above window×MTU ÷ response-size. |
| `-init-poll-delay D` | 100ms | initial idle poll interval (lower = snappier resume). |
| `-max-poll-delay D` | 10s | idle poll backoff ceiling. |
| `-stream-buffer N` | 1048576 | Client smux per-stream buffer; with `-bbr`, defaults to `-bbr-max-flight` unless explicitly set. |
| `-smux-recvbuf N` | 4194304 | Client session receive buffer; with `-bbr`, defaults to twice `-bbr-max-flight` unless explicitly set. |
| `-max-streams N` | 0 | max concurrent tunneled streams (`0` = unlimited). Backpressures the local accept loop at capacity — a runaway client (BitTorrent, a crawler) can't flood the single shared session and starve interactive/DNS streams. |

## Taming a connection-flooding client (e.g. BitTorrent)

The whole tunnel is **one** KCP/smux session over one DNS-over-TCP connection.
A client that opens hundreds/thousands of simultaneous connections (BitTorrent
peer discovery is the classic case) turns each into a smux stream over that one
narrow pipe — starving DNS and browsing behind the flood and blowing up memory
(`-stream-buffer` × N streams). The session itself survives, but interactive
traffic stalls.

`-max-streams N` caps the concurrency: at N in-flight streams the client stops
accepting, so extra connections wait in the kernel backlog (the app feels
backpressure) instead of piling onto the session. It does **not** make the
flooding client fast; it keeps the flood from
destabilizing everything else. A value like `-max-streams 32`..`64` leaves
plenty for normal browsing while capping a runaway client. (Better still: also
throttle the torrent client's global connection limit and disable DHT/µTP.)

## How to tune a new link
1. **Window:** measure RTT (ping) and target bandwidth. BDP = bw × RTT.
   Set `-kcp-window ≈ 2-3 × BDP ÷ 1400`. Too high → bufferbloat (bursty, stalls);
   too low → can't fill the pipe. (11 Mbps × 52 ms ⇒ BDP ~71 KB ⇒ window ~128.)
2. **Download size:** raise server `-mtu` (TCP only) until throughput plateaus;
   ~16384 is usually the knee.
3. **Finding the max-safe upload** (`-tcp-upstream-mtu`): start at 300, step up
   (500, 800, 1100…), watching the server log for `cannot parse DNS query`.
   The largest size with *no* parse errors and steady upload is your value.
   Many middleboxes truncate around a classic boundary (~512), so 500 is a good
   first guess.
4. **Latency-sensitive (browsing):** keep the window modest and `-init-poll-delay`
   low; huge windows help bulk transfer but hurt interactivity on slow links.
5. **Adaptive control: `-bbr`** (client only). Upload delivery counts unique
   KCP payload acknowledged by the server; download delivery counts unique
   incoming KCP payload. ACK sizes therefore cannot masquerade as upload
   capacity. Each direction maintains its own bandwidth and RTT model. Window
   targets are twice bandwidth × max(RTT, 100ms), constrained by the segment
   and byte budgets above. The 100ms headroom covers the pinned KCP default
   flush interval; it does not change measured RTT or the pacing estimate. Upload payload pacing runs at the DNS carrier. Optional fair-upload
   scheduling retains fairness, with admission shaping on slow paths and
   carrier-only pacing at high measured upload rates.
   Every upload data segment consumes pacing credit, including retransmissions.
   Control-only packets and download polls can pass while data waits for credit.
   A blocked underlying transport write can still delay all traffic.

   Brief zero-delivery feedback gaps do not erase the startup bandwidth seed.
   Active upload outages back off the estimate after eight seconds without
   delivery, then restart discovery when delivery resumes. The existing KCP
   profile switches to fast retransmission and immediate ACKs on sustained
   upload degradation. This is BBR-inspired interval-rate control, not a full
   implementation of TCP BBR: ACK compression and application-limited bursts
   can distort estimates. RTT modeling currently uses a 10ms floor and 2s
   ceiling. Download has no explicit remote-backlog signal.

   The poll target covers the bounded download flight window using unique
   payload per response, bounded
   by the baseline and 4096 outstanding queries. ACK-only and duplicate replies
   do not dilute the payload estimate. Rate-file legacy fields describe download;
   `UP_BW_BPS` and `UP_SUSTAINED_BPS` describe upload (all in bytes/second).

   There is no 500 Mbps limit or guarantee of maximum throughput on every path.
   Large bandwidth-delay products may require larger flight and smux budgets;
   explicit buffer flags remain authoritative. KCP's 65535-segment wire limit,
   small QNAME payloads, poll limits, CPU and transport overhead still constrain
   throughput. The byte budget bounds the controller's flight target, not total
   process memory: queues, retransmissions, streams and per-client state cost
   additional memory. Size server capacity for concurrent clients.

   Validation uses in-memory KCP/smux and scripted transports, including loss
   recovery, asymmetric feedback, pacing and integrity regressions. Sandbox
   restrictions prevented live WAN throughput measurements; these settings are
   a starting point for measurement on the actual deployment.

## Notes
- Reliability fixes: fair scheduling now retains ownership of an outstanding
  smux write across scheduler timeouts, so yielding does not resend queued bytes.
  The default submission timeout is now 0: queued writes wait through congestion, and producers block when the queue budget is full. Closing a flow or scheduler releases these waiters. A positive submission timeout explicitly opts into failing starved streams.
- The client rebuilds the transport, KCP, Noise and smux session after terminal
  session failure. Old application connections close; new connections use a
  fresh session. This is recovery, not transparent resumption of old streams.
  Noise handshakes are bounded to 30 seconds. TCP frame reads and writes are
  bounded to 90 and 30 seconds respectively. Established-session liveness still
  uses smux's two-minute keepalive timeout (its periodic checks can take longer
  to detect loss); reopening a transport retries with 1–8 second backoff.
- Reliable TCP server replies apply backpressure instead of silently dropping when the response queue fills. Empty polls have their own outstanding budget; upload queries and ACKs cannot consume that budget. Query timings are registered before transport emission to handle fast responses.
- The first measured RTT replaces the startup seed, and tiny application exchanges do not lower the bandwidth estimate. Upload demand is tracked before transport pacing, with rejected queue entries removed.
- Query expiry and history cleanup run even without `-bbr`. Closing a tunnel
  also closes its transport, stops DNS workers, and cancels HTTP/TLS work.
- Server and client must run the matching build (the `-tcp` EDNS carrier is
  understood by both). Keys are unchanged from upstream (`-gen-key`).
- Over `-tcp` the KCP substrate is reliable/ordered (TCP handles loss), so a
  large static window is safe *up to* the bufferbloat point — size to the link.

## Variable-uplink validation

The portable production-stack simulator in [tests/uplink](../../tests/uplink/README.md)
exercises slow and fast carriers, independent directional outages, ordered
TCP-style stalls, compressed feedback, mixed traffic, TLS, and bounded queues.
See [test results](UPLINK_TEST_RESULTS.md) for measurements and exclusions,
[profiling results](PROFILING_RESULTS.md) for measured bottlenecks and
optimizations, and [current literature](UPLINK_RESEARCH.md) for primary references.
