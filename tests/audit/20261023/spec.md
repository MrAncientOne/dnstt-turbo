# Spec-axis audit, 2026-10-23

Official reference: release `v1.20260501.0`, commit `0c5c52a57d899c05428c116898941761a2ed83c2`. Both findings concern newly added components absent from official dnstt. Production source and existing tests were not changed.

## P2: Live rate updates forgive all admission-shaper debt

Contract: `TURBO_README.md:107` promises “slow paths retain admission shaping before smux”; lines165–171 promise slow-path burst protection.

`fairqueue/fairqueue.go:318` replaces the entire shaper, clearing future clocks. The client calls `SetRate` every100ms (`dnstt-client/main.go:408,427`), even if the rate is unchanged. Thus a backlogged slow path may release a full fair chunk per ticker update instead of waiting for its actual serialization time. This defeats the additional admission protection and can increase downstream buffering/interactive delay; the separate carrier pacer still exists, so this is not a total removal of bandwidth control.

Two reproductions fail3/3: pending4-second debt becomes0; a live writer immediately receives its queued second4096-byte chunk after reapplying1024B/s. An isolated counterfactual preserving the clocks passes both reproductions plus existing static shaper coverage3/3 under the race detector.

## P2: Supported small server segments permanently use wrong receive-window units

Contract: configurable server `-kcp-mtu-cap` (`TURBO_README.md:89`) and adaptive bounded independent windows. Production server accepts KCP MTU200 (payload176) through `dnstt-server/main.go:1080`.

`dnstt-client/adaptive.go:63` ignores every observed payload below256, leaving1376-byte window units. Actual `Feedback` reports176; at1.76MB/s/100ms the controller advertises256segments =45,056 actual bytes rather than its intended352,000-byte flight. The1376-byte control case correctly reserves2,752,000bytes. This under-reserves flight and can limit downloads when `-bbr` meets a small segment configuration. It is not a measured WAN throughput claim.

Reproduction fails3/3; an isolated counterfactual accepting observed segment sizes passes it and all four existing adaptive tests3/3 under the race detector. The counterfactual is causal evidence, not a production-ready patch.

## Reproduction

Run from `/workspace/dnstt-turbo` with `GOPROXY=off GOMODCACHE=/home/agent/.cache/go-mod GOCACHE=/workspace/dnstt-audit/go-cache`:

- `/usr/local/go/bin/go test -overlay /workspace/dnstt-audit/audit-20261023/spec/shaper-overlay.json ./fairqueue -run 'TestAudit(SetRate|LiveRate)' -count=3 -v`
- `/usr/local/go/bin/go test -overlay /workspace/dnstt-audit/audit-20261023/spec/small-segment-overlay.json ./dnstt-client -run TestAuditSmallDownstreamSegmentsSizeActualFlight -count=3 -v`

Diagnostic green overlays: `shaper-candidate-overlay.json`, `small-segment-candidate-overlay.json` in this directory. These findings require optional adaptive/fair-upload flags and do not establish the cause of the user's current static-mode DNS behavior.
