# Throughput profiling: 2026-10-10-profile4

Follow-up: [400 Mbps target check](DOWNLOAD_400_CHECK.md) records unchanged-code warmed download-only runs at 408–420 Mbps, per-direction carrier overhead measurements, and workload-dependent results below 400 Mbps. These do not replace the original D/U/D measurements below.

[Upload follow-up](UPLOAD_PROFILE_CHECK.md) records 20 fast experiments and six constrained/mixed comparisons. No tested alternative demonstrated a consistent improvement, so production settings remain unchanged.

[Flaky-download follow-up](FLAKY_DOWNLOAD_CHECK.md) records impaired-path goodput, timeout experiments and a correction to peer queue sizing in the simulator. With matching startup queues, warmed fast download reached 418 Mbps with no internal queue drops; slower/flaky cases still leave useful capacity unused. Earlier measurements below used the smaller shared queue fixture.

This build removes measured allocation and scheduling costs without changing the DNS wire format. It retains directional carrier pacing, bounded queues, fair scheduling and slow-path admission protection. A finite simulation matrix cannot establish maximum throughput on every uplink.

## What the profiles established

The baseline was `2026-10-09-uplink3`. The configured fast carrier was 500 Mbps per direction, 10 ms one-way delay, a 4 MiB carrier queue, upstream MTU 1400 and server KCP window 65535. A 64 MiB download/upload/download sequence transfers 192 MiB of application data. Profiles include both peers and the simulator in one process; cumulative allocations are not resident memory, and summed goroutine blocking time is not wall time.

| Measured cost | Change | Regression evidence |
|---|---|---|
| Server query receive loop allocated a 64 KiB scratch buffer per query; approximately 9.55 GiB of the baseline allocation profile | Reuse one receive buffer; parsed names/RDATA and queued packets retain their owned copies | Allocation test went from 67,677 to 1,788 bytes/query; buffered-query identity and payload tests pass |
| Poll-inflight accounting scanned all live polls for every invocation; about 8% cumulative CPU in the initial profile | Track a conservative oldest-live timestamp and scan only when expiry is possible | At 4096 polls, approximately 60–68 microseconds became 22–26 nanoseconds; tests cover expiry boundaries, cancellation, RTT changes and late replies |
| Name validation encoded a temporary name solely to check its length | Count encoded length directly and reuse computed suffix strings | Validation went from 22 allocations to zero; existing boundary/error/wire tests pass |
| TXT bundles repeatedly grew intermediate buffers | Size and validate TXT data before allocating, and reuse server bundle assembly scratch | A 16,027-byte bundle went from 8 encode / 7 decode allocations to one each; ownership, boundary and differential wire-format tests pass |
| Fast upload paid for a second per-chunk admission clock in front of an already paced carrier | Bypass only this clock after actual sustained fast upload delivery; retain fair scheduling and carrier pacing | High/low/hysteresis controller tests; a fast/slow/fast production-stack transfer verifies recovery and exact bytes |

The admission threshold is derived from the fair chunk size divided by one millisecond. At the default 4096-byte chunk, bypass requires non-app-limited delivered upload at a sustained rate of at least 4.096 MB/s. Admission shaping returns below 2.048 MB/s. Startup seeds and downstream bandwidth cannot enable the bypass. This is a timer-amortization policy, not full kernel BBR.

The final 192 MiB profile recorded **4,355.90 MiB cumulative allocations**, versus **15,588.82 MiB** in the baseline (approximately 72% less). Total sampled CPU was 18.14 seconds versus 24.90 seconds, across both peers and the harness. These are whole-run observations with different packet counts, not a deterministic performance ratio. The last TXT/scratch change reduced the intermediate profile's allocations from 5,262.83 to 4,355.90 MiB; the targeted allocation tests provide the stronger isolated evidence.

The final CPU profile's largest individual function was KCP `flush` (13.62% flat, 14.66% cumulative). Runtime waiting/scheduling and copies also remain substantial; deterministic simulator payload generation itself accounted for 6.73% cumulative CPU. The simulator's roughly 492 Mbps download / 260 Mbps upload bare-carrier result already demonstrates host timer/packetization costs, especially for 1400-byte uploads. It is not a kernel TCP line-rate baseline.

## Controlled experiments and rejected alternatives

Final unprofiled 64 MiB D/U/D repeats in the configured 500 Mbps case:

| Run | First download Mbps | Upload Mbps | Second download Mbps |
|---|---:|---:|---:|
| 1 | 249.938 | 103.909 | 306.446 |
| 2 | 259.345 | 101.598 | 312.594 |
| 3 | 248.247 | 101.279 | 313.080 |

Baseline repeats recorded 204.6–265.7 Mbps first download, 84.4–94.7 Mbps upload, and 214.4–291.3 Mbps second download. Startup and host scheduling materially affect finite transfers. An intermediate build reached 323–340 Mbps second download; the final repeats above are the current reproducible evidence, so the TXT allocation reduction is not claimed as a proven isolated throughput improvement.

The separate 500-to-5-to-500 Mbps uplink case completed its first upload at 22.080 Mbps over the entire impaired interval and its final upload at 72.235 Mbps after recovery. The race run also completed and passed its recovery threshold. All application bytes matched; these case averages are not instantaneous rates or a guarantee for real outages.

All component experiments used frozen sources and serial benchmark processes. In three matched runs before the later DNS allocation changes, retaining the second upload admission clock gave 82.7–88.4 Mbps upload; unconditional bypass gave 101.9–105.2 Mbps. But on a 1 Mbps upload / 100 Mbps download carrier, unconditional bypass reduced upload from 0.527–0.533 to 0.478–0.479 Mbps. The final implementation therefore uses measured capacity and hysteresis rather than unconditional bypass.

Turning upload fairness off reached 121.5 Mbps in one fast experiment, but sacrifices the required sharing behavior and was rejected. Enabling immediate KCP ACKs on both peers reduced fast download to 184/202 Mbps in one experiment and was also rejected. Setting upstream MTU 500 reduced fast upload to 49.4 Mbps versus 82.7–88.4 with MTU 1400. These are controlled-case observations, not universal configuration rules: a real middlebox may require the smaller MTU.

The server's static KCP window matters on fast paths: a matched 1024-segment case reached 134/138 Mbps download versus roughly 249–315 Mbps with 65535. This does not establish that 1024 is a problem on a 10 Mbps link, or justify changing a live server without comparison. The client's adaptive window cannot raise a server-imposed static send-window ceiling.

## Validation scope and practical limits

The final matrix passed **58/58 runs**: 28 configured cases plus a 60-second changing-capacity soak, each in a normal build and a race build. The final profile and three longer unprofiled fast repeats also passed. The earlier pre-TXT matrix passed 56/56, but final acceptance uses the complete rerun after the last production changes.

The tests run production DNS framing, polling, KCP, Noise, smux, directional controllers and fair queues through bounded pipe carriers. They check byte integrity, stream isolation, transfer deadlines, shutdown, reliable reply delivery and carrier queue bounds. Cases include 64 kbps through configured 2 Gbps capacities, asymmetry, high RTT, jitter, repeated capacity changes, short/long blackouts, ordered TCP-style stalls, mixed traffic, TLS negotiation, idle restart, churn and slow readers. The fast/slow/fast case adds a 500-to-5-to-500 Mbps uplink transition.

TXT differential fuzzing completed 146,492 cases. The supported package race suite passed 255 tests/subtests with two explicit socket-dependent skips; separately, 23 supported fair-queue tests (37 tests/subtests) passed. Five fair-queue tests requiring OS sockets remain unavailable in this sandbox: `TestWriteDeadlineRequeues`, `TestStalledWriterKeepsFlowsAlive`, `TestIntegrityTCP`, `TestChurn2000`, and `TestSparseLatencyUnderLoad`. The simulated stack supplies additional byte-integrity, churn and interactive-load coverage, but does not replace those real-socket tests.

Actual Linux TCP, Wi-Fi behavior, kernel CAKE/ECN, tun2socks, SOCKS negotiation and Internet paths are outside this simulator. `tc` and `perf` are unavailable; Go's pprof source was available and used to build a local reporting tool. Large queues, very low rates and TCP head-of-line stalls still produce poor latency or goodput in some passing cases. Passing means the specified integrity/recovery/regression checks succeeded, not that every case approached line rate.

For example, the final normal-build mixed-load cases had tiny-request p95 of 2383.3 ms with a 32 KiB carrier queue and 1787.7 ms with a 1 MiB queue. The TLS-during-bulk case had 326.7 ms p95. These measurements remain visible because maximum bulk throughput and interactive latency are separate goals; no universal latency guarantee is claimed.

The remaining CPU work includes KCP packet processing/flush, DNS parsing/encoding, timers, goroutine scheduling and copying between protocol layers. Encryption was not the leading CPU cost in the intermediate fast profile. Reducing these further requires another measured change and regression cycle; these results do not prove a global optimum.

Both rebuilt peers are needed to deploy all improvements: receive/bundle scratch reuse is server-side. Changing only the client deploys the poll accounting, upload admission policy and client codec improvements, but cannot apply the server fixes.

## Reproduce and inspect

```sh
python3 tests/uplink/run.py --mode performance --case fast-500m --size-mb 64 --profile
python3 tests/uplink/run.py --mode performance --case fast-500m --size-mb 64 --repeat 3
python3 tests/uplink/run.py --mode both --protocol-only --soak-seconds 60
```

Profiling adds overhead. Use unprofiled repeats for throughput comparisons and avoid competing CPU-heavy processes. Runner artifacts retain source hashes, settings, per-transfer rates, logs and raw profiles. See [harness instructions](../../tests/uplink/README.md), [earlier validation](UPLINK_TEST_RESULTS.md), and [primary-source research](UPLINK_RESEARCH.md).

Evidence in this workspace lives under `/workspace/dnstt-audit`: `profile-uplink3-fast`, `profile-uplink3-baseline`, `profile-experiments`, `profile-slow-compare`, `profile4-final-drop`, `profile4-final-drop-race`, and the `profile4-final-*` validation directories. An earlier candidate matrix was intentionally interrupted after nine cases and is not counted as final validation.
