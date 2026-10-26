# Server / transport audit findings — 2026-10-23

Audited current `/workspace/dnstt-turbo` source; no production files modified.
Official comparison: `/workspace/dnstt-audit/audit-20261023/official`, release
v1.20260501.0, commit `0c5c52a57d899c05428c116898941761a2ed83c2`.
No repository-specific standards document was found. Findings below are concrete
behavior/resource regressions, not Fowler-smell judgements.

## P1: unauthenticated poll IDs allocate about 3 MiB each at default settings

Locations: `dnstt-server/main.go:101` (kcpWindow=65535), `:1253`
(SetQueueSize(window*2)), `:854-855` (Unstash/OutgoingQueue for any decoded
ClientID), `turbotunnel/remotemap.go:134-139` (eager per-ID channel allocation).

A valid empty query with a new 8-byte ClientID requires neither a KCP data
packet nor a Noise handshake. The real recvLoop decodes it and sendLoop creates
its remote record. Default channel capacity is 131070 slice entries (~3 MiB).
The 32-TCP-carrier cap does not bound the number of IDs on one carrier; UDP
source addresses need not differ either. IDs remain until remote expiry
(4-minute timeout with 2-minute sweeps). A server can exhaust memory through a
small number of unauthenticated queries, and normal reconnect churn also costs
far more memory than upstream.

The isolated test feeds eight wire queries through real recvLoop/sendLoop:
- turbo default: ~25,169,376 bytes retained after GC (24 MiB);
- turbo queue128 control: ~27–36 KiB;
- official release default128: ~36 KiB.
Three repeated native runs and three race runs reproduce the difference.
The native threshold intentionally goes red above2MiB; no production fix was
applied. The incoming queue's initial allocation is excluded, making this a
per-ID incremental measurement.

## P2: one blocked TCP reply delays unrelated clients

Locations: `dnstt-server/main.go:945` (single synchronous send loop),
`dnstt-server/tcp.go:203-207` (blocking framed WriteTo with30-second bound).

Once an accepted client's outbound kernel buffer fills because it stops reading,
the one global sendLoop waits on that client's WriteTo. A healthy client's
already queued response cannot be serviced until that write finishes or expires.
No per-client output worker isolates backpressure. An unauthenticated client can
induce DNS error replies; no completed Noise session is required. The32-carrier
limit does not remove this starvation.

The minimized test supplies two actual transport connections via net.Pipe
(which models an already-full outbound buffer), queues an error reply for the
blocked peer before the healthy peer, and shows the healthy reply cannot arrive
within100ms. Releasing the blocked peer immediately unblocks delivery. This is
not a claim that a real kernel buffer fills after one small response: repeated
replies are required there. Three native/race repetitions fail identically.
Official dnstt has no TCP listener and thus no equivalent slow-reader surface.

## P1/P2: accepted smaller EDNS cap can make every large segment unparseable

Locations: `dnstt-server/main.go:629-642` (relaxed cap admission), `:1073-1081`
(listener-wide KCP segment size), `:890-898` (first oversize segment permitted),
`:934-941` (raw truncation to this query's advertised size).

At serverMax16384, the configured KCP MTU is1400. A response to an accepted
query advertising1232 contains1478 wire bytes with that segment. sendLoop slices
it to1232 and setsTC, leaving incomplete TXT RDATA; the client's real DNS parser
returns unexpectedEOF. Retransmitting the same1400-byte segment cannot repair
this. The same occurs at512. Small handshake/control frames may succeed,
misleadingly allowing a session to start before large downstream data stalls.
A4096-byte advertised control passes.

The direct-TCP default client advertises65535 and is unaffected. This impacts
mixed-size UDP resolver support and the documented bare `-mtu16384` dual-listener
recipe (or smaller-than-listener limits). Separate listener caps
`-mtu1232 -tcp-mtu16384` avoid the1232 case; general per-client MTU negotiation
is not implemented. Official default1232 uses KCP MTU932 and emits1008-byte
responses for the equivalent segment test, passing3/3. The fork fails3/3 under
native and race instrumentation. Severity depends on whether such clients are
supported/deployed; this is not evidence of the user's direct-TCP Codex delay.

## Reproduction

From current repo:

```sh
env GOPROXY=off GOMODCACHE=/home/agent/.cache/go-mod \
 GOCACHE=/workspace/dnstt-audit/go-cache /usr/local/go/bin/go test \
 -overlay=/workspace/dnstt-audit/audit-20261023/standards/overlay.json \
 ./dnstt-server -run '^TestAudit' -count=3 -v -timeout=30s
```

Add `-race` for repeated race-instrumented probes. These newly added audit
assertions intentionally fail; existing tests are untouched.

From official snapshot, substitute `official-overlay.json` and run the same
command without the TCP-only probe (only two applicable tests are injected).

Artifacts: `audit_server_test.go`, `overlay.json`, `repro.log`, `mtu-repro.log`,
`official_allocation_test.go`, `official-overlay.json`, `official-repro.log`.
