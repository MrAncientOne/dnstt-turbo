# Download speed during impairments: 2026-10-11

Download is not established as maximum across flaky uplinks. The tests demonstrated byte integrity and recovery, but also exposed unused modeled capacity, redundant retransmission work and workload-dependent goodput. A targeted timeout experiment improved some cases and failed to improve others consistently. Production code and binaries remain `2026-10-10-profile4`; only the simulation harness was corrected.

## A benchmark correction

The original harness bypassed the peers' CLI startup queue sizing and set a shared 2048-packet queue in the client fixture. Both peers run in one process, whereas deployed peers have separate globals. Receive-queue construction could also precede that shared setting. Fast tests therefore did not faithfully reproduce the production startup policy.

Instrumentation found **6476 outgoing internal packet drops** in a fast baseline, with 6836 KCP retransmissions, including 6473 early retransmissions. These are recoverable KCP datagram drops, distinct from dropped DNS server replies. They are not evidence of real network packet loss. Slow stable/jitter instrumented runs had zero internal incoming/outgoing drops, but thousands of timeout retransmissions.

`tests/uplink/prepare.py` now freezes queue capacity independently for each fixture peer: twice the adaptive client ceiling (or static client window), and twice the configured server window. Fixture-only constructors preserve that capacity when remote send queues are allocated. Production files and public APIs are unchanged. This models the existing startup policy; it is not an increase to deployed queue sizes.

With matching peer queues, the warmed 128 MiB fast download reached **418.384 Mbps**, with **zero internal queue drops**. KCP still recorded 1869 timeout retransmissions over the three-download sequence. Previous fast measurements remain valid as observations of their fixtures, but must not be treated as exact production queue behavior or a maximum-speed ceiling.

## Speed comparisons

All figures are useful application Mbps averaged over complete transfers. The carrier reference transfers raw records through the same delay/capacity/queue model using 16000-byte download chunks. It excludes DNS, KCP, Noise, smux, reverse protocol feedback, outer TCP and OS/WAN behavior. It is a packetization-specific reference, not an achievable end-to-end VPN speed guarantee.

| Case | Carrier reference Mbps | Current code, matching peer queues Mbps |
|---|---:|---:|
| Stable 10 Mbps, 50 ms one-way delay, 16 MiB download | 9.960 | 6.591 |
| Same link with ordered 0–80 ms downstream jitter | 9.923 | 6.388 |
| 10 Mbps cellular capacity cycle, 8 MiB download | 5.958 | 3.917 |
| 500→5→500 Mbps downstream collapse, 128 MiB download | 76.439 | 72.695 |

The cellular cycle provides two seconds at configured capacity, two at one eighth capacity, a half-second blackout, then full capacity until the six-second cycle repeats. The collapse changes at three and fifteen seconds; an intentional 2.5-second delay before bulk transfer ensures the download spans the impairment. These averages include impaired time and depend on how much of each phase the transfer encounters. Neither the 72.695 Mbps collapse average nor the reference's 76.439 Mbps is an instantaneous recovered-path rate.

Before the queue correction, additional matched-carrier investigations measured:

| Case | Carrier reference Mbps | Tunnel Mbps |
|---|---:|---:|
| 10→1→10 Mbps downstream, low-capacity interval at 2–5 seconds | 8.298 | 5.679 |
| Three-second downstream blackout | 8.147 | 5.730 |
| Three-second upstream feedback blackout | 9.960 | 5.530 |
| 1 Mbps feedback uplink / 100 Mbps downlink, bounded 256 KiB queues | 38.627 | 10.697 |

The raw reference does not require reverse feedback, so the feedback-blackout and asymmetric rows are deliberately loose comparisons. The 100 Mbps reference is itself constrained by its finite queue and delay. These rows use the old queue fixture and are labeled accordingly; the current-policy table above is the more faithful configuration comparison.

One-second application and delivered-carrier samples were recorded. In the feedback blackout run, useful download resumed in the first sampled second after service returned at five seconds, then approached the preceding stable rate in the following second. Completion/recovery succeeded while subsequent goodput remained below the carrier reference. In the original cellular run, full-capacity periods commonly delivered only approximately 6.4–6.9 Mbps useful traffic; recovery does not imply full utilization.

## Testing the retransmission theory

No deliberate carrier record loss was configured in the stable and jitter cases. Instrumented stable/jitter runs also reported zero internal packet drops. They nevertheless recorded thousands of KCP timeout retransmissions. The KCP loss counter denotes inferred loss, not independently measured network loss. This supports delayed-feedback/timeouts as one source of wasted work in those cases.

Two alternatives were tested in frozen fixtures:

1. Shorten only the initial client KCP update/ACK batch interval to 10 ms. This did not produce a consistent improvement and was rejected.
2. Change the KCP minimum RTO from 100 to 500 ms in a **local dependency copy**, leaving the cached dependency and production build untouched. This nearly eliminated stable/jitter timeout retransmissions, but did not consistently maximize goodput.

Earlier smaller-queue experiments with the higher RTO floor reached 7.756 Mbps stable download, 6.780–7.546 Mbps jittered download and 4.874–4.995 Mbps cellular-pattern download. Baseline jitter repeats varied substantially, from 4.691 to 6.778 Mbps. These limited serial samples do not establish statistical significance or a portable percentage gain.

After applying the matching peer-queue policy to both alternatives:

| Case | Current code Mbps | Experimental RTO floor Mbps |
|---|---:|---:|
| Jittered 10 Mbps | 6.388 | 5.497 |
| Cellular-pattern 10 Mbps | 3.917 | 4.703 |
| Final warmed fast download | 418.384 | 418.473 |

The higher-floor jitter run had only two KCP retransmissions and zero queue drops, yet its useful rate was lower. Avoiding retransmissions alone therefore does not eliminate the remaining window/polling/feedback/scheduling limits. The cellular result improved, and the fast result was effectively unchanged in these samples. A blanket timeout change is not demonstrated as a consistent improvement and was **not adopted**.

The experimental higher floor also completed a ten-second blackout and deliberate dropped/duplicated-record recovery in the earlier fixture, preserving exact bytes. Dropped-record goodput was 3.519 Mbps versus 3.622 Mbps baseline in one comparison. Those synthetic losses are separate from the normal ordered TCP impairment model. An eventual implementation would need transport-specific policy and broader acceptance checks, not a global dependency-constant edit.

## Reliability checks and remaining poor cases

**49/49 investigative runs passed**, comprising 41 production-stack fixture runs and eight bare-carrier references. These include original comparisons, repeated cases, timing alternatives and matching-queue comparisons. The corrected repository harness then passed **16/16 checks** across eight scenarios in normal and race builds: slow 64 kbps, fast 500 Mbps, asymmetric feedback, cellular changes, long download blackout, simultaneous bulk/tiny traffic, TLS during bulk, and record loss/duplication. This is selected rerun coverage, not a claim that the entire previous matrix was rerun.

Passing production-stack tests require exact application bytes, completion within deadlines, shutdown, zero dropped reliable server replies and bounded carrier queues. Race-build rates are not throughput evidence. No real TCP sockets, Wi-Fi, tun2socks, CAKE or WAN behavior was tested here.

The corrected normal-build mixed case still had **2022.2 ms tiny-request p95**. The long-downstream-blackout D/U/D case completed at 1.027 / 0.622 / 2.478 Mbps, and the 64 kbps case's final download averaged only 0.015 Mbps. These are visible limitations: byte integrity and eventual recovery passed, while goodput and interactive latency remain poor in some conditions. The tested threshold does not redefine those rates or delays as optimal.

## Reproduce and inspect

The corrected repository validation command was:

```sh
python3 tests/uplink/run.py --mode both --protocol-only \
  --case fast-500m --case cellular-both --case long-download-blackout \
  --case mixed-small-queue --case tls-during-bulk --case record-loss-bursts \
  --case slow-64k --case asymmetric-1up-100down
```

The instrumented comparison script is `/workspace/dnstt-audit/download_flaky.py`. It freezes the module and records one-second useful/delivered-wire samples, settings, input hashes, logs, KCP counters and internal queue-drop counters. Results are under `download-flaky*`; corrected harness validation is under `download-queue-fix-validation`. Fixture modifications and dependency experiments are outside the production tree. Initial stable/variable sample logs had endpoint direction labels reversed; their JSON sample fields were corrected and marked `sample_direction_corrected`. Useful application byte counts were unaffected, and later fixtures use the correct direction labels.

The deployed-binary source hashes still match `dnstt-turbo-bin/BUILD_INFO.json`. The practical conclusion is **tested recovery with measurable throughput limits**, including a reproducible cellular-pattern improvement worth further work, rather than maximum download speed in every flaky condition.
