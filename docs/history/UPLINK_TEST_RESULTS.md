# Variable-uplink validation: uplink3

Build stamp: `2026-10-09-uplink3`. Validation completed across 9–10 October 2026. Configured carrier rates are emulator settings, not achieved VPN rates. Sources and settings are frozen and hashed for each run.

## Confirmed bugs and fixes

1. **Window too small for batching on fast, short-RTT paths.** The pinned KCP version defaults to a 100ms flush interval. A flight target based only on a measured 20ms RTT exhausted flight before the next service interval. The new adaptive regression test failed before the fix (291 segments and 29 polls at a 10 MB/s sample). Adaptive clients now reserve flight using `max(RTT, 100ms)`, retaining segment/byte bounds and unchanged measured RTT/pacing. The poll target covers this bounded receive window using observed payload per response. Generic controller users retain the default zero extra horizon.
2. **A short feedback gap erased the initial bandwidth estimate.** One active-demand interval with no delivered payload replaced the startup seed with zero. A regression test reproduced this directly. Zero-rate intervals now do not enter the delivery-rate maximum filter. Sustained active outages still back off after eight seconds; the regression verifies both behaviors. Existing positive delivery samples and application-limited protection remain active.

Both changes are client-side. The shipped server retains the previously implemented reliable reply backpressure; replacing only the client activates these new controller fixes. Deployment of the matching server is needed to obtain the server's earlier backpressure fixes if the remote server does not already include them.

## Coverage and acceptance

The production DNS/KCP/Noise/smux/controller/fair-queue stack runs over bounded, independently impaired in-memory carriers. Tests validate seeded per-stream bytes, upload acknowledgment, ordering, duplicate handling, bounded carrier queues, zero reliable-server reply drops and bounded shutdown. Mixed workloads include simultaneous upload/download and delayed tiny requests, idle restart, slow receivers, connection churn, and real TLS 1.3 negotiation/certificate verification. TLS application payloads are synthetic, not an HTTP implementation. Native and race modes run separately; race goodput is not a production speed measurement.

The suite covers 64 kbps–configured 2 Gbps, 1:100 asymmetric rates, RTT through one second, recurring cellular-style capacity changes, blackouts, ordered stalls, batched downstream feedback, contention modeled as reduced service, and small/deep queues. Record loss/duplication is explicitly separate from outer TCP IP loss: TCP normally repairs losses and exposes an ordered stall.

## Native protocol measurements

These are short finite transfers, including startup. Sequential cases show download/upload/download payload Mbps; concurrent cases show completion rates for each bulk stream. A zero p95 means that case did not measure tiny requests. These are single runs, not confidence intervals.

| Case | Bulk Mbps | Tiny-request p95 ms |
| --- | --- | --- |
| slow-64k | 0.048/0.027/0.036 | 0.0 |
| slow-256k | 0.172/0.175/0.204 | 0.0 |
| slow-1m | 0.775/0.362/0.751 | 0.0 |
| regression-static | 5.351/5.968/6.283 | 0.0 |
| moderate-10m | 5.396/4.340/5.824 | 0.0 |
| fast-100m | 68.255/41.628/75.085 | 0.0 |
| fast-500m | 139.760/73.492/218.747 | 0.0 |
| fast-2g | 141.368/62.216/260.323 | 0.0 |
| asymmetric-1up-100down | 17.535/0.542/26.653 | 0.0 |
| asymmetric-100up-1down | 0.492/3.134/0.770 | 0.0 |
| high-rtt | 0.385/0.669/0.515 | 0.0 |
| cellular-uplink | 2.522/2.530/2.343 | 0.0 |
| cellular-both | 3.010/2.735/3.078 | 0.0 |
| tcp-hol-stalls | 1.804/0.697/1.272 | 0.0 |
| ack-compression | 2.710/3.652/1.866 | 0.0 |
| upload-blackout | 2.003/2.853/3.378 | 0.0 |
| download-blackout | 1.916/3.094/3.351 | 0.0 |
| background-contention | 5.234/2.157/4.501 | 0.0 |
| mixed-small-queue | 1.750/1.694 | 2302.4 |
| mixed-deep-queue | 3.159/3.133 | 1445.5 |
| slow-reader | 2.074/1.449/1.425 | 1030.1 |
| idle-restart | 1.357/2.306/1.594 | 0.0 |
| churn-80 | tiny only | 102.3 |
| record-loss-bursts | 3.192/1.259/3.438 | 0.0 |

Mixed tiny-request p95 of about 1–2.3 seconds remains a practical latency limitation, despite passing the 4-second regression threshold. Passing integrity and completion does not mean all throughput or latency objectives are met. The asymmetric slow-feedback case also shows substantial upload restriction; the controller cannot make the reverse path disappear.

A before-fix fast-500m experiment delivered approximately 27.7/5.5/5.3 Mbps. The verified fixed native run delivered 139.8/73.5/218.7 Mbps for the same 8 MiB D/U/D configuration. This identifies a serious self-imposed fast-link limit and shows a material improvement. It is not evidence that a 500 Mbps WAN will deliver those numbers, nor that the fixed implementation reaches the physical maximum. The 2 Gbps configured case remains far below line rate; CPU, timers, protocol overhead and finite workload size matter.

## Bare-carrier calibration

The original bare-carrier reference used 1,400-byte chunks in both directions and plateaued around 250 Mbps on this host. Actual tunnel download bundles are much larger; therefore earlier tunnel/raw fractions near or above 100% do not establish physical link efficiency. Those evidence files are retained without rewriting their metrics.

The revised reference uses 16,000-byte download chunks and MTU-sized upload chunks (500 or 1,400). It records integrity and timing separately through `--raw-only`. This fixes the most obvious packetization mismatch but is still a simulator reference: no kernel TCP ACKs, real congestion controller, or exact DNS overhead. Do not compare its fractions with the earlier reference or call it a WAN throughput ceiling.

## Environment limits and follow-up

`tc` is unavailable in this sandbox. No actual kernel TCP/netem, competing CUBIC flows, ECN/AQM, Wi-Fi drivers, tun2socks, SOCKS application negotiation, CAKE routing or WAN measurements are claimed. Source substitution removes OS listener/backend dial seams and supplies a scripted carrier. Carrier-level resource checks do not bound total process memory, including KCP retransmissions, application buffers and concurrent client state. The soak is one minute, not a long coexistence or memory-leak qualification.

Use receiver-ingress netem on real hosts, adjacent direct/tunnel/direct measurements, saved impairment seeds, separate warmup/measurement, and staggered competing flows over several minutes. Literature includes convergence taking over four minutes in one BBRv3 experiment, so short tests cannot settle coexistence. Research guidance and dated primary links are in [UPLINK_RESEARCH.md](UPLINK_RESEARCH.md); reproduction commands and acceptance thresholds are in [tests/uplink/README.md](../../tests/uplink/README.md).

## Evidence locations

- `/workspace/dnstt-audit/uplink-verified-v3`: 24 native cases and the race cases completed before interruption; immutable source snapshot, settings, logs, environment and results.
- `/workspace/dnstt-audit/uplink-resumed-v3`: remaining race cases after interruption, using the revised bare reference. Production controller code is unchanged.
- `/workspace/dnstt-audit/uplink-extra-v3`: TLS during bulk and 60-second changing-uplink soak, native and race.
- `/workspace/dnstt-audit/uplink-v3-suite.json`: 275 passing supported unit/subtest cases, two self-skips. Five socket-dependent fairqueue cases were explicitly excluded because local socket execution is unavailable here: `TestWriteDeadlineRequeues`, `TestStalledWriterKeepsFlowsAlive`, `TestIntegrityTCP`, `TestChurn2000`, `TestSparseLatencyUnderLoad`.
- `/workspace/dnstt-audit/flight-candidate.log`, `fast-static-reference.log`, `feedback-gap-candidate.log`, and `uplink-trace.log`: isolated diagnostic experiments; not additional final-matrix cases.

## Final validation totals

All **59 production-stack executions passed**: 28 scenario variants in normal/race modes (56 runs), plus three repeated native fast-link runs. All three separately calibrated bare-carrier runs passed. The supported race unit suite passed 275 test/subtest cases, with the exclusions listed above. `go vet` on the controller, client and server, Python compilation, build/architecture checks and checksum verification also passed.

Three independent fixed native fast-500m repetitions delivered:

| Repetition | Initial download Mbps | Upload Mbps | Final download Mbps |
| --- | --- | --- | --- |
| 1 | 148.042 | 69.423 | 222.715 |
| 2 | 136.898 | 73.506 | 237.339 |
| 3 | 135.283 | 72.970 | 227.302 |

Ten-second upload blackouts passed in native and race modes. Native D/U/D rates were 1.009/0.627/2.472 Mbps. Ten-second download blackouts also passed both modes, with native rates 1.002/0.762/2.242 Mbps. Those rates include interruption and rediscovery; recovery is visibly slower than the short-blackout cases. Completion alone does not establish optimal recovery.

The separately calibrated raw native final-download rates were 96.910, 460.161 and 1480.371 Mbps for configured 100/500/2000 Mbps cases respectively. MTU-sized raw uploads were 96.509/252.550/258.510 Mbps. This exposes the host's small-record processing/timing ceiling and avoids the earlier incorrect interpretation of a 250 Mbps raw download plateau.

Additional artifacts: `uplink-fast-repeat-v3`, `uplink-long-blackout-v3`, `uplink-raw-calibration-v3`, and `uplink-combined-v3.json` under `/workspace/dnstt-audit`. Static Linux client amd64, server amd64 and server arm64 builds are in `/workspace/dnstt-turbo-bin`, with `SHA256SUMS` and source/build metadata in `BUILD_INFO.json`.


## Matched fast-link comparison with fixes disabled

A separate immutable fixture disables only the two new fixes and restores RTT-only poll sizing. It retains all earlier recovery fixes and uses the same latest payload/ACK harness, source inputs, fast-500m settings, and native compiler as the fixed repetitions. Runs are sequential without another benchmark running. This is a controlled comparison against deliberately reverted controller logic, not a test of an old deployed binary. All three before-fix runs completed with correct bytes, but reproduced the throughput limit:

| Repetition | Initial download Mbps | Upload Mbps | Final download Mbps |
| --- | --- | --- | --- |
| 1 | 31.229 | 5.526 | 4.847 |
| 2 | 30.004 | 5.519 | 4.896 |
| 3 | 32.801 | 5.505 | 4.906 |

Evidence: `/workspace/dnstt-audit/uplink-before-repeat-v3`; deliberate changes and modified-file hashes are saved there. These diagnostic reverted runs are additional experiments, not counted among the 59 passing final production-stack executions. The experiments implicate controller logic independently of a changing real uplink, without implying universal field performance.
