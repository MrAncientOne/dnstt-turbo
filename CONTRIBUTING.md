# Contributing

Start with [Development](docs/DEVELOPMENT.md), the
[architecture and source map](docs/ARCHITECTURE.md), and
[upstream relationship](docs/UPSTREAM.md). Use the focused guides in
[docs/README.md](docs/README.md) rather than treating historical reports as the
current configuration reference.

Keep patches focused: explain the observed problem, the behavior after the
change, and how it was verified. For a bug, reproduce the failure at the real
caller seam before fixing it when possible. For throughput changes, compare
the same workload and retain integrity, completion, and recovery checks.
Report simulator measurements separately from real-network measurements.

Before submitting:

```sh
make check
make race
```

Run [targeted transport or simulation checks](docs/TESTING.md) when relevant.
Describe skips and environmental limits in the pull request. Update the
owning guide when flags, protocol behavior, or development commands change.

Retain the upstream package structure, local smux patch, and copyright and
license notices. Keep generated binaries in release assets, and deployment
choices in separate operator tooling. Review `git diff --cached` before a
commit; this repository's prepared checkout may already contain staged work.

For an issue, provide the binary SHA256, compiler/platform, transport, relevant
flags, a minimal reproduction, and redacted logs. Keep server private keys and
proxy credentials out of reports. See [Logging](docs/LOGGING.md) for interpreting
health and session events.
