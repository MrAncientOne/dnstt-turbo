# Testing

## Routine checks

```sh
make check
make race
```

`check` runs Go formatting validation, relative Markdown link checks, Python
release-tool tests, root and local smux module tests, and vet for both modules.
`race` runs both Go suites with race instrumentation. Tests involving OS sockets
need a host that permits local listeners; a sandbox permission failure is not
a failed protocol invariant. Record skips and environmental failures honestly.

The root suite exercises nine packages; the nested `third_party/smux` module
has its own tests. Each suite retains its ordinary package layout.

## Focused regressions

| Area | Source / command |
|---|---|
| TCP reply isolation, limits and burst recovery | `go test ./dnstt-server -run TestReplyQueue -count=3`; [slow-reader regression](../dnstt-server/audit_regression_test.go) |
| Small EDNS caps, bounded errors, peer allocation | [server regressions](../dnstt-server/audit_regression_test.go), [error encoding](../dnstt-server/error_response_test.go), [resource limits](../turbotunnel/resource_limits_test.go) |
| Cipher record failure and session rebuild signals | [Noise failures](../noise/failure_test.go), [client session failure](../dnstt-client/session_failure_test.go) |
| Stream-registration ordering | `go test ./fairqueue -run TestSmuxRegistersStreamBeforeSendingSYN -count=3` |
| Scheduler rate changes and integrity | `go test -race ./fairqueue -count=1` |
| Directional delivery and pacing | `go test ./bbr ./dnstt-client -count=1` |
| DNS parser fuzzing | `go test ./dns -run '^$' -fuzz FuzzMessageFromWireFormat -fuzztime=30s` |

Run targeted package tests while editing, then the broader required checks.
Tests that exercise the actual frame/stream seam are stronger than a test that
only reproduces arithmetic from the implementation. A successful TCP connect
or `session_ready` event does not assert that application bytes were delivered.

## Controlled production-stack simulation

Download dependencies first (`go mod download`). The harness defaults to
offline module resolution for reproducible runs; it can use the populated
cache. A short static control is:

```sh
make uplink-smoke
```

Select scenarios explicitly:

```sh
python3 tests/uplink/run.py --mode both --protocol-only \
  --case regression-static --case tls-during-bulk --repeat 2
```

For the complete matrix and a changing-capacity soak:

```sh
python3 tests/uplink/run.py --mode both --soak-seconds 60
```

Read the [harness guide](../tests/uplink/README.md) for options and evidence
formats. It freezes current sources into a generated module, checks exact
replacement seams, and exercises production DNS/KCP/Noise/smux and optional
controllers/queues with synthetic application payloads. It records sources,
settings, completion, integrity, rates, queue peaks, logs and environment.

The harness substitutes the carrier and application listener/dial seams. It
does not run the real OS TCP carrier, SOCKS negotiation, tun2socks, a Wi-Fi
driver, Linux CAKE, or a WAN. Real TCP framing still needs the package-level
carrier tests. The recorded throughput of a simulated carrier is not a claim
about achievable physical link speed. Race runs measure correctness rather
than production throughput. Run performance cases without competing CPU-heavy
jobs and compare the same sizes/settings.

## Historical fixtures

[tests/audit/20261023](../tests/audit/20261023/README.md) keeps frozen reproducers
for the earlier audited snapshot and provenance for an official dnstt control.
On current source, its runner selects the promoted regression tests:

```sh
python3 tests/audit/20261023/run.py --race --repeat 3
```

Those historical assertions intentionally fail on the old snapshot. Compiler,
download or sandbox failures are not evidence that the assertion reproduced.

## Field evidence

Operators can compare application transfers through their own TCP backend.
Record binary SHA256, full settings, byte counts, error output, wall times, and
session continuity. Keep endpoint or direct-path failures separate from tunnel
failures. Test both directions and new streams after uploads, concurrent bulk
traffic, and sustained usage. A finite passing run establishes only its own
scope. Gateway/deployment benchmark scripts are kept separately from this
source repository because their routing, endpoints and service choices are
operator-specific.
