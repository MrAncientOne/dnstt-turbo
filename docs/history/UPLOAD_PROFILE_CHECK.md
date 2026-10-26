# Upload investigation, 2026-10-11

Upload is not established as optimal. This investigation used unchanged `2026-10-10-profile4` production sources and isolated fixture variants. No production defaults or binaries were changed: none of the alternatives demonstrated a consistent throughput improvement across the checked workloads.

## Fast upload measurements

The carrier was configured for 500 Mbps in each direction, 10 ms one-way delay, 4 MiB carrier queues, MTU 1400, server KCP window 65535, client adaptive control and fairness on both peers. Each run transferred 64 MiB three times in upload/upload/upload order, checking exact bytes and zero dropped reliable server replies. Alternatives ran serially, two repetitions each. Host and startup variation is substantial; these small samples cannot establish statistical significance.

| Fixture alternative | First upload Mbps, two runs | Final upload Mbps, two runs |
|---|---|---|
| Unchanged baseline | 68.285 / 100.564 | 106.771 / 120.318 |
| Increase burst cap from 16 to 64 KiB | 86.216 / 60.051 | 119.191 / 101.523 |
| Disable scheduler admission clock, retain carrier pacing/fairness | 99.761 / 96.063 | 106.141 / 109.310 |
| Suppress ACK-only response-triggered polls when feedback is available | 59.102 / 78.986 | 113.471 / 109.930 |
| Avoid simulator timers when the packet's delivery time has already passed | 88.822 / 82.723 | 120.986 / 123.268 |
| Disable carrier pacing, retain adaptive windows and admission policy | 67.919 / 61.541 | 82.751 / 64.871 |
| Limit Go parallelism to two processors | 99.659 / 85.319 | 110.343 / 124.774 |
| Disable client adaptation and use static 65535-segment windows | 145.069 / 144.340 | 148.083 / 145.576 |
| Enable immediate ACKs on the server only | 63.892 / 97.174 | 78.250 / 101.314 |
| Double adaptive window gain from two to four | 110.169 / 110.655 | 117.837 / 125.191 |

The static-window alternative is a composite diagnostic: disabling adaptation also removes adaptive carrier/admission pacing. It does not isolate window size as the cause. Individual pacing removal and window-gain experiments failed to reproduce its isolated-upload advantage consistently. Disabling carrier pacing can still leave the admission shaper active, so it is not an unlimited whole-stack reference.

## Profile and framing evidence

A separate profiled baseline U/U/U run reached **103.904 / 124.586 / 121.006 Mbps**. It recorded 11.63 seconds of sampled CPU over 14.53 seconds wall time, including both peers and the simulator. CPU costs included runtime futex work (10.49% flat), memory copies (6.02% flat), KCP flush (5.67% cumulative), allocation (15.05% cumulative), DNS message parsing (9.54% cumulative), and client DNS send (14.45% cumulative). These overlapping cumulative percentages must not be added together.

The blocking profile recorded approximately 1.22 seconds attributed directly to the client's upload-pacer select and approximately 6.21 seconds cumulative in DNS send. Blocking totals across goroutines are not wall-clock percentages. Removing pacing did not improve throughput, so the profile is not evidence that removing that wait would recover the difference to line rate.

Each warmed profiled upload delivered 67,108,864 application bytes while sending approximately 77.37 million upstream DNS carrier bytes: roughly **86.7% upstream payload efficiency**. Downstream feedback cost another approximately 8.08 million carrier bytes per upload. Counters include DNS framing, protocol/control/retransmission traffic and length prefixes, but omit outer IP/TCP headers and kernel TCP behavior. Feedback demand matters particularly on asymmetric links.

Thus framing overhead alone cannot explain 100–125 Mbps useful upload on a configured 500 Mbps carrier. The previous bare-carrier upload calibration itself reached approximately 260 Mbps with 1400-byte chunks; it is packetization-specific and omits protocol work. It must not be presented as a physical line-rate baseline. The timer-only fixture change above did not demonstrate a large gain, so simulator timers alone also do not explain the remaining gap.

The evidence points to interacting delivery/window control, small-packet processing, scheduling and feedback costs. It does not identify a single proven bottleneck whose removal yields maximum upload speed.

## Constrained and mixed-load checks

Three configurations were compared once per case: unchanged baseline, doubled window gain, and the static 65535-window configuration. These are diagnostic comparisons, not a full reliability acceptance matrix.

On a configured 1 Mbps upload / 100 Mbps download link, with 50 ms one-way delay, MTU 500, 256 KiB carrier queues and a 512 KiB isolated upload:

| Configuration | Useful upload Mbps |
|---|---:|
| Baseline | 0.359 |
| Window gain four | 0.371 |
| Static configuration | 0.743 |

For simultaneous 1 MiB upload, 1 MiB download and twelve tiny exchanges, with 10 Mbps directional carriers, 50 ms one-way delay, MTU 500 and 32 KiB queues:

| Configuration | Upload Mbps | Download Mbps | Tiny p95 ms |
|---|---:|---:|---:|
| Baseline | 1.814 | 1.899 | 2011.3 |
| Window gain four | 1.539 | 2.034 | 2195.0 |
| Static configuration | 1.122 | 1.337 | 1586.6 |

The static alternative improved isolated upload and tiny latency here, but worsened simultaneous bulk throughput. Increasing adaptive window gain also did not improve this mixed upload or interactive latency. None is a demonstrated all-workload improvement. The passing four-second interactive regression threshold is not a claim that two-second interactive latency is good.

## Validation and artifacts

All **20/20 fast experiment runs** and **6/6 constrained/mixed runs** completed with exact bytes and zero dropped server replies; the separate profiled run also passed. No capacity-change or blackout matrix was rerun because no implementation change was adopted. Existing production reliability coverage and exclusions remain in [the profiling report](PROFILING_RESULTS.md). These pipe tests do not exercise actual Linux TCP, Wi-Fi, SOCKS negotiation or tun2socks.

Reproduction scripts and evidence are retained under `/workspace/dnstt-audit`:

- `upload_experiments.py`: default baseline/burst/admission experiments; passing a variant name selects that fixture variant, e.g. `python3 upload_experiments.py window-gain4`.
- `upload_constrained.py`: slow/asymmetric and mixed checks using the frozen compiled fixtures.
- `upload-experiments`: individual settings, results, source hashes, frozen production inputs and logs. Source hashes describe inputs before explicitly recorded fixture edits.
- `upload-baseline.log`, `.cpu.pprof`, `.block.pprof`, `.cpu.txt`, `.block.txt`: profile evidence.

Next optimization work should instrument actual flight occupancy, application-demand classification and packet counts during matched upload runs, then test a targeted change with fast/slow capacity transitions and mixed traffic. The present investigation rejects several simple knob changes; it does not prove the remaining upload rate is the maximum.
