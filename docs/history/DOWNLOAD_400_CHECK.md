# Checking the 400 Mbps download target

Measured on 2026-10-11 using unchanged `2026-10-10-profile4` production sources. The target is achievable in this pipe simulator, but is not a guaranteed sustained rate across workloads or real links. No production defaults or binaries were changed for this investigation.

## Conditions and results

The carrier was configured for 500 Mbps per direction, 10 ms one-way delay, a 4 MiB queue, MTU 1400, server KCP window 65535, client adaptive control and upload/download fairness. Both peers and the carrier run in one process. Sources were frozen by the existing harness; extra instrumentation sampled carrier counters before and after each sequential transfer. The counters include DNS records and their two-byte length prefixes, but exclude IP/TCP headers, kernel TCP ACKs and real TCP retransmissions. Per-transfer deltas may include trailing protocol traffic from the preceding stream.

| Unchanged-code workload | First download Mbps | Upload Mbps | Later downloads Mbps |
|---|---:|---:|---:|
| 64 MiB D/U/D, repeat 1 | 338.759 | 59.764 | 371.379 |
| 64 MiB D/U/D, repeat 2 | 347.046 | 58.616 | 379.710 |
| 128 MiB D/D/D, repeat 1 | 352.148 | — | 408.304 / 411.051 |
| 128 MiB D/D/D, repeat 2 | 364.354 | — | 415.475 / 409.008 |
| 256 MiB D/D/D | 387.706 | — | 420.385 / 420.449 |
| 128 MiB D/U/D | 357.492 | 60.666 | 387.860 |

Every listed run completed, checked exact application bytes, and reported zero dropped server replies. These are whole-transfer averages, not instantaneous samples. D/U/D is sequential; it does not measure simultaneous bidirectional performance. The 256 MiB warmed downloads each took approximately 5.1 seconds, so this is not a multi-minute sustained-speed result.

## Overhead versus unused capacity

The two default 128 MiB download-only repeats delivered 134,217,728 application bytes per transfer while sending approximately 143.9–145.1 million downstream carrier bytes. Payload efficiency was **92.52–93.28%**, corresponding to roughly **6.72–7.48% non-payload carrier bytes**. This measures all included downstream framing/control/retransmission costs, not just DNS headers. The upstream feedback traffic must be considered separately, particularly on asymmetric links.

Applying that measured downstream efficiency to a fully utilized 500 Mbps carrier gives a conditional **463–466 Mbps** framing-only estimate. It is not an implementation speed prediction: processing, polling, scheduling, startup and actual outer TCP can leave additional capacity unused. Thus an assumed 20% download overhead is too pessimistic for these bundled-response cases, while claiming 400 Mbps in every situation is unsupported.

The 128 MiB upload in D/U/D used 154,889,610 upstream carrier bytes, approximately 86.7% upstream payload efficiency, but reached only 60.666 Mbps. Framing alone plainly does not account for that shortfall. Current upload measurements were lower than the previous day's 101–104 Mbps runs; no production code changed between them. Host/workload sensitivity and remaining pacing/processing costs prevent treating either set as a portable maximum.

## Controlled alternatives

Holding the download workload constant, setting `GOMAXPROCS=2` reached 409.699/410.337 Mbps warmed download; `GOMAXPROCS=4` reached 413.142/413.068 Mbps. Reducing only the server KCP update interval to 10 ms reached 413.6–415.3 Mbps over two repeats. These overlap the unchanged-code results and do not establish a meaningful improvement. Neither setting was adopted.

A 512 MiB D/D/D attempt completed two byte-exact transfers at 385.688 and 393.085 Mbps, then its process received SIGKILL. It is incomplete and excluded from successful-run counts. The fixture retains multiple whole-payload buffers and available host memory became low; memory pressure is a plausible explanation, but an OOM cause was not independently confirmed. The subsequent 256 MiB run completed. This failure does not establish a production tunnel defect or production memory bound.

## Evidence and reproduction

The existing harness command for the unchanged D/U/D comparison is:

```sh
python3 tests/uplink/run.py --mode performance --case fast-500m --size-mb 64 --repeat 2 --protocol-only
```

The isolated instrumentation and one-variable experiments are retained in `/workspace/dnstt-audit/check_target400.py`; longer-transfer confirmation is in `/workspace/dnstt-audit/check_target400_long.py`. Results, settings, frozen sources, source hashes and logs are under `/workspace/dnstt-audit/target400-experiments`; the original D/U/D repeats are under `/workspace/dnstt-audit/target400-baseline`. Experiments ran serially. No new reliability matrix is claimed because no production implementation changed; see [the existing validation and profiling report](PROFILING_RESULTS.md) for reliability scope and remaining limitations.

The evidence supports **400 Mbps as demonstrated for warmed download-only simulations**, with workload-dependent results below it as well. Actual Linux TCP, Wi-Fi, SOCKS/tun2socks and WAN validation remains necessary to establish real deployment performance.
