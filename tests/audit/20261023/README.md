# Regression reproducers from the 23 October 2026 audit

**24 October fixes:** the regression tests are now promoted into their Go
packages and run in `go test ./...`. On that source, the runner below selects
those tests directly and should exit 0. Unsupported EDNS sizes are explicitly
rejected; accepted sizes must carry whole segments. The original `.go.txt`
fixtures and findings below describe the pre-fix snapshot and remain unchanged
for historical comparisons.

These are **intentionally failing assertions** against the audited turbo source.
They cover five distinct defects using six test functions. They do not fix them.
The `.go.txt` fixtures keep these probes separate from the ordinary Go suite.
`run.py` materializes temporary Go files and injects them into the real packages
using `go test -overlay`; production files and existing tests are never edited.

From the repository root, with the project's Go dependencies already cached:

```sh
python3 tests/audit/20261023/run.py --race --repeat 3
```

An exit code of 1 is expected on the audited version. Read each Go assertion:
a compile/setup failure is not proof of a regression. `--group server`,
`--group shaper`, and `--group small-segment` isolate the three packages.
`GO_BIN`, `GOMODCACHE` and `GOCACHE` can select the local compiler and caches.
The runner defaults to `GOPROXY=off`; set it explicitly if dependency downloads
are needed. Use `--output DIR` to retain logs and generated overlays; otherwise
a new temporary directory is retained and printed.

The server tests bound their own memory use: eight IDs and roughly 24 MiB of
incremental retained heap. The slow-reader test uses `net.Pipe` to represent an
already-full socket buffer; it does not claim that one response fills a real
kernel buffer. No external server, credentials, routing or firewall changes are
needed. All probes use local objects or local pipes.

## Official controls

Unpack official release `v1.20260501.0` and run:

```sh
python3 tests/audit/20261023/run.py --group official \
  --official /path/to/unpacked/official --repeat 3
```

The official controls cover default per-ID allocation and its normal 1232-byte
response configuration. They passed three repetitions. Official dnstt does not
have the new TCP listener, adaptive controller or fair scheduler, so there is
no direct official test for those features. The precise release commit and
download hash are in [upstream.json](upstream.json).

## Results and scope

- [Server and transport findings](standards.md): three confirmed regressions.
- [Adaptive and fair scheduler findings](spec.md): two confirmed regressions.
- [Full audit](../../../docs/history/AUDIT_2026-10-23.md): upstream drift, dependency
  alerts, validation and limitations.

All three packaged turbo groups failed their intended assertions in three
race-instrumented repetitions. This is different from the existing suite,
which passed normally and with the race detector. Race instrumentation was used
to stress concurrency; the five findings are invariant failures, not new data
race reports. Changes to production settings or code may change the verdict.
