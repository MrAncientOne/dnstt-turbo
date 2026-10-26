# Working on dnstt-turbo

This is a fork of dnstt, not a gateway distribution. Keep the upstream-style
`dnstt-client/`, `dnstt-server/`, `dns/`, `noise/`, and `turbotunnel/` layout.
The root Go module path is deliberately retained for existing internal imports.

## Read by task

- Before changing packet flow, ownership, concurrency, or recovery, read
  [Architecture](docs/ARCHITECTURE.md) and its links to the owning source files.
- Before changing flags, defaults, transport selection, or MTU handling, read
  [Configuration](docs/CONFIGURATION.md) and [upstream compatibility](docs/UPSTREAM.md).
- Before changing scheduling or adaptation, read
  [Scheduling and flow control](docs/SCHEDULING.md).
- For reproductions, test selection, and performance claims, read
  [Testing](docs/TESTING.md). Historical reports in
  [docs/history](docs/history/README.md) describe their recorded snapshots.
- For dependencies, retain the ordering fix documented in
  [third_party/smux/PATCHES.md](third_party/smux/PATCHES.md) and the notices in
  [Licensing](docs/LICENSING.md).
- For builds or GitHub artifacts, read [Releasing](docs/RELEASING.md).

## Changes

Preserve tested runtime behavior during documentation or repository cleanup.
Keep refactors local to the owning package and exercise the actual caller seam.
New gateway, routing, firewall, systemd, or host-specific automation belongs in
a separate deployment repository. The Go `cake` and `fairqueue` packages are
built-in tunnel code; they do not configure Linux qdiscs.

Preserve these reliability contracts:

- Admit whole KCP segments into DNS responses; reject incompatible response
  caps explicitly instead of slicing an encoded response.
- Bound per-peer queues and resource allocation before authentication. Empty
  polls must not allocate a peer's window-sized packet queues.
- Keep each TCP carrier's reply order and lifetime independent. Queue-full
  admission rejects the new unsent datagram; already admitted replies stay
  intact. Actual I/O failures and deadlines still terminate the carrier.
- Treat Noise record/integrity failure as terminal. Recover with a fresh
  session rather than continuing a desynchronized cipher stream.
- Register smux streams before publishing SYN. Keep cancellation and clean
  close from overtaking admitted data in fair queues.

## Validate and hand off

Use the Go toolchain in [go.mod](go.mod). Run `make check`; run `make race`
for Go behavior/concurrency changes and before release. The patched smux is a
nested module, so root `go test ./...` alone does not test its own suite.
Use the focused tests and simulator commands in [Testing](docs/TESTING.md)
when the change affects a transport or scheduler. Record environment-related
skips and failures separately from assertion failures.

Run `make docs-check` after moving Markdown files. Build with `make release`
when changing release tooling; verify `dist/latest/SHA256SUMS`. Keep binaries,
credentials, captures, logs, and generated simulator modules out of source Git.
Use stable binary filenames in `latest` and SHA256 to distinguish builds.

Report what changed, the commands run, their outcomes, and material limits.
Keep the existing Git history and remotes; publication, tags, and releases are
separate actions performed when the user requests them.
