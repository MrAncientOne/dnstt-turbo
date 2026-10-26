# Phase: Multipath — direct TCP:53 primary + public-resolver secondaries (FUTURE, not implemented)

Status: design doc only. No code changed. Single-path behavior is unchanged.

Goal: keep direct DNS-over-TCP:53 as the bulk carrier when reachable, add
public-resolver paths (DoH/DoT/UDP) for failover, tail-latency duplication,
and bounded extra aggregate — without ever letting a slow path drag the fast
path down.

Non-goals for this phase: custom ARQ transport replacement, kernel `tc`,
server-side independent estimator, resolver-path stealth improvements.

## 1. Where we are (single-path constraint)

* Client selects exactly one transport: `dnstt-client/main.go:693-770`
  (`-doh` / `-dot` / `-udp` / `-tcp`, exits if more than one given).
* One `DNSPacketConn` per run (`dnstt-client/dns.go:147`,
  `dnstt-client/dns.go:446`), one KCP conn (`dnstt-client/main.go:256`),
  one Noise session, one smux session.
* Server runs one isolated session space per listener (`TURBO_README.md:15`):
  sessions never span `-udp` / `-tcp` listeners today.
* Download pipeline is already estimator-driven on the single path:
  live `pollTarget` (`dnstt-client/dns.go:63`), BDP-sized depth
  (`dnstt-client/dns.go:100`), ID-matched RTT (`dnstt-client/dns.go:187`),
  per-tick BBR update + window/pacer/shaper feed
  (`dnstt-client/main.go:392-432`).

So multipath is an architecture change on both ends, not a flag. The
sections below ground what that change must respect.

## 2. Literature and standards grounding

### 2.1 Multipath transport architecture

* **MPTCP, RFC 8684** (https://www.rfc-editor.org/info/rfc8684/): simultaneous
  use of multiple subflows between peers. Key mechanism is the Data Sequence
  Signal (DSS): subflow sequence numbers mapped to one connection-level Data
  Sequence Number. Loss/reordering is per-subflow; reassembly is at connection
  level. Direct lesson for us: KCP today has a *single* sequence space, so
  striping segments across heterogeneous DNS paths without a DSS-like
  connection-level map reproduces MPTCP's head-of-line (HoL) problem. Either
  add a connection-level sequence above per-path KCP sessions, or keep one KCP
  session and accept that the scheduler + receive window must absorb all skew.
* **Coupled congestion control, RFC 6356** and follow-ups LIA / OLIA / BALIA:
  MPTCP couples subflow windows so a multipath flow is no more aggressive than
  one TCP on the best path (fairness goal). Our goal is the opposite — a
  private tunnel that takes everything it can — so **uncoupled per-path BBR**
  is the correct choice here. Cite RFC 6356 only to explicitly reject coupling
  for this use case, and to borrow its stability analysis (flapping, fairness
  to self across paths).
* **Multipath QUIC, draft-ietf-quic-multipath-21**
  (https://datatracker.ietf.org/doc/draft-ietf-quic-multipath-21): simultaneous
  paths in one connection, **one packet-number space per path**, loss detection
  and congestion control per path (QUIC-RECOVERY per-path). Packet-number
  debate result (De Coninck et al. 2021/2022): per-path spaces are more robust
  to receiver ACK heuristics. Direct lesson: our per-path monitor must keep
  per-path ID/timing spaces (we already avoid in-flight ID reuse,
  `dnstt-client/dns.go:251`; extend that to per-path pending maps), and RTT +
  bandwidth estimation must be per path, never mixed.
* **QUIC base**: RFC 9000 (transport, streams, flow control, migration),
  RFC 9002 (loss detection, RTT measurement, NewReno-style CC signals),
  RFC 9250 (DoQ stream mapping — one transaction per bidirectional stream,
  concurrency guidance). Relevant because SlipStream carries QUIC over DNS and
  inherits these properties/limits.

### 2.2 Schedulers: how to not be bound by the slowest path

* **MinRTT (default in MPTCP/MPQUIC)**: send on lowest-RTT path with open cwnd.
  Simple, near-optimal on homogeneous paths, suffers HoL on heterogeneous ones.
* **ECF — Earliest Completion First** (Lim et al. 2017,
  https://www.repository.cam.ac.uk/bitstreams/9f216be1-4124-4f10-bbad-137e912cc7ff/download):
  schedule each chunk where it is predicted to *arrive* first, accounting for
  queued bytes / path bandwidth + RTT. Slow path gets bytes only when that
  actually advances completion. This is the mechanism that lets aggregate
  approach the sum without being dragged to the minimum.
* **BLEST — Blocking Estimation scheduler** (Ferlin et al. 2016,
  https://dl.ifip.org/db/conf/networking/networking2016/1570234725.pdf):
  skip a slow path when using it would block (HoL) the fast path — i.e. when
  waiting for fast-path cwnd to open completes earlier. The complementary guard
  to ECF; both exist because heterogeneity + small receive windows cause
  degradation.
* **Redundant / reinjection**: duplicate stream bytes across paths to minimize
  completion under loss (at 2x cost). Right tool for interactive/small/tail
  traffic, wrong tool for bulk.
* **Stream-aware + learning variants**: SA-ECF (Rabitsch et al. 2018), Peekaboo
  (learning-based). Note only as future work; ECF + BLEST + redundant covers
  the practical space.
* Linux-kernel analysis notes (Cech thesis; Simula/BLEST pages) agree on the
  failure mode: heterogeneity causes reordering → limited receive window →
  stalls. Remedy is always scheduler + sufficiently large receive window
  (MPTCP lesson: window must cover sum of path BDPs plus skew).

Implication for turbo: per-path BDP estimators + ECF/BLEST poll (download)
and query (upload) distribution + summed receive sizing
(`smuxRecvBuf`, KCP windows, `turbotunnel` queue). Single-stream latency still
follows the path its bytes took — hence interactive pinned to the fast path.

### 2.3 Congestion control model

* **BBR** (Cardwell et al., CACM 2017; spec draft-ietf-ccwg-bbr,
  https://datatracker.ietf.org/doc/draft-ietf-ccwg-bbr/): model-based —
  estimate bottleneck bandwidth (windowed-max delivery rate) and RTprop
  (windowed-min RTT), hold in-flight at their product, pace just under the
  estimate. Startup/Drain/ProbeBW/ProbeRTT gains are the literature values our
  `bbr/` already follows. BBRv2/v3 add loss/ECN signals and startup exits; our
  `KCPProfiler` degraded verdict + `ExitStartupEarly` is the tunnel analogue.
* Multipath use: one controller per path (own `maxBWpts`, `minRTT`,
  `pacingRate`, `windowSegs`/`sendSegs`). Never mix samples across paths with
  different propagation delays — the damped min-RTT expiry and 8s max window
  in `bbr/bbr.go` assume one path's distribution.

### 2.4 AQM / fairness (already in-tree)

* **CoDel, RFC 8289** (https://www.rfc-editor.org/info/rfc8289/); **FQ-CoDel,
  RFC 8290** (https://www.rfc-editor.org/info/rfc8290/): sojourn-based signal,
  per-flow isolation, new-flow priority by behaviour. Our `cake/cobalt.go`
  ports the control law + invsqrt cache; `fairqueue/` ports sparse-vs-bulk
  (new/old) + DRR++ deficit.
* **CAKE** (Høiland-Jørgensen et al.; design arxiv 1804.07617; reference
  `third_party/cake/sch_cake.c`): BESTEFFORT single-tin + deficit shaper
  (`cake_set_rate`), Cobalt (CoDel + BLUE), buffer sizing `rate*interval/4`
  floor 4MiB, longest-queue drop. Our shaper follows BBR SustainedBW at 0.9x.
  Multipath extension: keep one CAKE instance (total ordering into smux) but
  feed it per-path-shaped arrivals; or shape per path and order globally.
  Either way the *total* shaper target is the sum of path estimates, not any
  single path's.

### 2.5 DNS transports and resolver limits (the binding constraints)

* Wire: RFC 1035 (names, 255-octet limit), RFC 6891 (EDNS(0)), RFC 7766
  (DNS-over-TCP framing + ID matching — our monitor already implements this),
  RFC 9210 (TCP operational guidance).
* Encrypted transports: RFC 8484 (DoH), RFC 7858 (DoT), RFC 9250 (DoQ).
  Operational note verified Sep 2026: **Quad9 retires DoH HTTP/1.1 on
  2025-12-15** (https://quad9.net/news/blog/doh-http-1-1-retirement/) —
  HTTP/2+ only afterwards.
* Rate limiting (primary sources):
  * Google Public DNS ISP docs
    (https://developers.google.com/speed/public-dns/docs/isp): per-IP QPS
    limits, REFUSED-on-throttle option, explicit advice to use alternative
    resolvers alongside Google.
  * Cloudflare 1.1.1.1 network operators
    (https://developers.cloudflare.com/1.1.1.1/infrastructure/network-operators/):
    typical use unlabeled, but uncacheable/SERVFAIL against one domain and
    proxied/scanning traffic may be limited to protect upstream auth.
  * Community-observed (non-authoritative, treat as order-of-magnitude only):
    Google ~100 qps historically, Cloudflare single-digit qps complaints on
    shared IPs, Quad9 non-single-number limits.
  * Authoritative RRL (BIND `rate-limit` token bucket, 15s window, `slip`
    truncated-retry-TCP) applies at our own server too.
* Throughput math that follows: tunnel needs ~60 queries/s per MB/s down at
  16KB responses. A resolver path realistically yields ~0.2-1 Mbps, not 10.
  Extra aggregate from resolvers is therefore +10-30%, never 2x the direct
  path. The big win is failover + tail latency, not sum-of-pipes.

### 2.6 Adjacent DNS-tunnel work (what each solved)

* **SlipStream** (https://github.com/EndPositive/slipstream,
  Rust https://github.com/Mygod/slipstream-rust,
  Go port https://github.com/getlantern/slipstream *minus multipath QUIC*):
  QUIC-over-DNS, QUIC CC for rate-limited resolvers, multipath QUIC combining
  resolvers even with different characteristics; published 10MB claims
  (~10x time, ~15% fewer queries up / ~37% fewer down vs baselines).
  Relevant if we ever return to resolver paths: their resolver-rate adaptation
  is the one problem in our space they solved and we have not.
* **VayDNS** (https://github.com/net2share/vaydns, CC0 dnstt fork; also vendored
  under `phoenix-vpn/vaydns`): leaner wire (`ClientID` 2B vs 8B, no padding
  prefixes, 255B vs 223B max data/query), downstream `-record-type`
  (`txt` highest capacity; `null` raw; `cname/ns/mx/srv/a/aaaa/caa` name- or
  chunk-bounded), `-max-qname-len`/`-max-num-labels`, `-rps` token bucket,
  per-query UDP sockets + forged-response filtering, `-dnstt-compat` interop.
  Static knobs only — no estimator, no adaptive poll depth, no FQ/shaper.
* **MasterDNSVPN** (https://github.com/masterking32/MasterDnsVPN, MIT):
  custom 5-7B ARQ, multi-resolver + duplication, latency/loss adaptive routing,
  8 balancing modes, MTU auto-discovery. Header win comes from replacing
  KCP+Noise+smux — explicitly rejected for turbo (reliability risk); DNS-layer
  trims are the portable subset.
* **KCP** (https://github.com/skywind3000/kcp,
  https://github.com/xtaci/kcp-go): 24B/segment header, stream mode,
  window/MTU discipline our stack relies on. **smux** (xtaci/smux): 8B frames,
  session/stream buffers. **Noise NK** (Noise spec): server-authenticated
  encryption independent of DoH/DoT.

## 3. Design for this phase (when taken up)

Modes in increasing complexity (do in order; stop when measured gain flattens):

1. **Active-standby failover.** Direct primary, resolver backup(s). Fast health
   checks (ID-matched RTT + delivery-rate stall detector already in
   `bbrMonitor`), fast switch on sustained collapse (existing `KCPProfiler`
   verdict). Biggest reliability win, minimal change.
2. **Redundant duplication for interactive/tail.** Duplicate sparse chunks and
   KCP tail retransmits on 2 paths; first answer wins. 2x query cost on a
   small fraction of traffic; bulk untouched.
3. **ECF/BLEST striping for bulk.** Per-path BBR (`MaxBW`, `MinRTT`,
   `PacingRate`, `SendWindowSegments`/`WindowSegments`), per-path pacer +
   per-path `pollTarget` (`PollDepthForBDP` per path with per-path
   `AvgResponse`), ECF poll/query distribution with BLEST skip. Server unifies
   sessions by ClientID across listeners; responses route back on the arrival
   path (DNS query/response correlation). Receive sizing = sum of path BDPs +
   skew headroom.

Asymmetries to preserve: upstream ratio (EDNS ~500B vs QNAME ~100B) differs
from downstream ratio (16KB vs ~1.2KB) — size each direction per path.
Covertness tradeoff: mixing direct (VPS IP visible locally) with resolver
(hides destination locally) links the paths by timing; document, do not solve
here. Rotation of direct IPs needs a control channel (cf. Phoenix remote
configs + `dnst://` URLs + `f35`/`dnst-scanner`) — separate work item.

## 4. Acceptance bar (measure, do not assert)

* Failover: kill direct mid-bulk; tunnel continues on resolver path with no
  session loss; recovery back to direct without restart.
* No-harm: with direct healthy and resolvers configured, bulk throughput >=
  direct-only (proves BLEST/ECF never drags).
* Aggregation: saturate direct; resolvers add their measured standalone
  capacity within ~20% (proves striping, not just failover).
* Tail: p99 interactive latency under bulk + one degraded path improves vs
  single path (proves duplication).
* Suite stays green: `go test ./...` including `cake/`, `fairqueue/`
  (churn-2000, integrity, sparse-latency), `dnstt-client/`, `dnstt-server/`,
  `bbr/`, plus new per-path estimator and ECF/BLEST unit tests.

## 5. References (canonical)

* RFC 8684 MPTCP; RFC 6356 coupled CC (LIA/OLIA/BALIA family).
* draft-ietf-quic-multipath-21; RFC 9000 / 9002 / 9250.
* ECF (Lim et al. 2017); BLEST (Ferlin et al. 2016); SA-ECF; Peekaboo.
* BBR CACM 2017; draft-ietf-ccwg-bbr.
* RFC 8289; RFC 8290; CAKE design + `sch_cake.c`.
* RFC 1035 / 6891 / 7766 / 7858 / 8484 / 9210 / 9250.
* Google Public DNS ISP docs; Cloudflare 1.1.1.1 operator docs; Quad9 DoH
  HTTP/1.1 retirement notice.
* SlipStream (EndPositive / Mygod-rust / getlantern-go); VayDNS (net2share);
  MasterDNSVPN; KCP; kcp-go; smux; Noise.

## 6. Sub-case: N direct IPs on one instance (e.g. OCI secondary IPs)

Same multipath framework as above, but near-homogeneous paths. Documented
separately because the physics — and therefore the expected gain — differ
from resolver secondaries.

### 6.1 What "multiple IPs, one instance" actually is

* OCI: one primary VNIC + primary private IP by default; secondary private
  IPs can be added to the same VNIC (same subnet), each optionally mapped to
  a reserved public IP
  (docs: "Assigning a New Secondary Private IP to a VNIC",
  "How to assign additional Public IP"; community how-to: add second private
  IP, attach reserved public IP). No extra VNIC required for a handful of
  IPs. VNIC count itself is shape-limited (official Compute Shapes doc: up to
  24 VNICs; bandwidth scales with shape/OCPUs).
* Server side needs nothing new: `NewTCPServerPacketConn(":53")`
  (`dnstt-server/tcp.go:54`) calls `net.Listen("tcp", address)`, so `:53`
  already accepts all local IPs on the box. Work is OS + cloud config: assign
  the secondary IPs on the VNIC, add them to the OS (`ip addr`), allow :53 in
  the security list / iptables, and publish the list to clients. No second
  `run()` needed — same listener, same session space.
* Client side (future): destination list instead of one `-tcp VPS_IP:53`.
  Resolver paths still need NS glue per name; direct mode needs no delegation
  at all, just the IP list (cf. `dnst://` URL / remote-config pattern in
  Phoenix).

### 6.2 Honest physics: what N IPs on one box give you (and what they do not)

* **Not extra link capacity.** All IPs share one NIC, one shape bandwidth cap,
  one upstream path and one censor middlebox on the client→server AS path.
  Official shape docs tie bandwidth to shape; community-observed caps (e.g.
  ~50 Mbps hard cap reports on OCI Ampere ARM free tier, ~480 Mbps VNIC
  descriptions on always-free) vary by shape/tenancy — measure the shape, do
  not assume per-IP additive bandwidth. If the bottleneck is the instance
  NIC or the middlebox byte cap, 4 IPs give 1x, not 4x.
* **Yes to per-IP-block resilience.** Censors block destination IP (:53 or
  SNI-less TCP:53 fingerprint + IP). N independent destination IPs means one
  block kills 1/N of capacity; client fails over in one RTT probe instead of
  full outage. This is the primary reason to do it.
* **Yes, sometimes, to policer-bucket scaling.** Middlebox policers
  (Juniper/Cisco token-bucket, RFC 2697 single/dual-rate) are keyed per flow
  (5-tuple) or per destination. If the throttle is per-destination-IP, N IPs
  mean N buckets and aggregate rises toward the link cap; if it is per-source
  (client IP) or link-rate, N buys nothing. Only measurement on the actual
  path tells which keying applies — probe one IP to throttle onset, then add
  the second and see whether total moves.
* **Yes to handshake racing / pinning.** Same-box IPs can still differ by
  several ms (ECMP member, conntrack bucket, ARP/ND state). Race SYNs
  (Happy-Eyeballs style), pin bulk to the winner, keep the rest as hot
  standby. Cheap, no scheduler needed.
* **No help against SNI-less TCP:53 fingerprinting itself.** If the censor
  throttles all TCP:53 by pattern (2-byte length prefix + DNS header), new IPs
  on the same pattern get throttled identically. That case calls for resolver
  paths (§3), not more direct IPs.

### 6.3 Design consequences (fits §3 with simplifications)

* Paths are near-homogeneous (same RTT ± ms, same BDP class), so MinRTT +
  failover already captures most of the gain; full ECF/BLEST still applies but
  rarely triggers. Keep per-IP BBR estimators anyway — per-IP blocking shows
  up as per-path collapse, and a mixed estimator would poison the healthy IPs.
* Poll/query distribution across direct IPs can be plain weighted round-robin
  by per-IP `MaxBW` (no ECF math needed until a resolver path joins the set).
* Server `maxServerTCPConns = 32` (`dnstt-server/tcp.go:51`) counts accepted
  sockets, not IPs — N client IPs from one client still consume one slot each
  during overlap; raise only if steady-state fan-out exceeds it.
* Glue/NS note (resolver mode only): multiple glue A/AAAA records under one
  `tns` name give resolver-side round-robin/failover (RFC 9471 glue rules);
  direct mode ignores all of this — the IP list is client config.

### 6.4 Acceptance bar (appended to §4)

* Block one of N direct IPs mid-bulk (iptables DROP on server for that IP):
  throughput dips ≤ 1/N and recovers without session loss; blocked IP is
  deprioritized by estimator within seconds.
* Single-IP vs N-IP bulk on an unthrottled shape: N-IP within ±5% (proves
  no-harm; extra IPs must never cost).
* Per-destination policer test: throttle onset per IP measured, then total
  with 2 IPs reported — documents whether this path keys per-destination
  (scales) or per-source/link (does not).
