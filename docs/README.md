# Documentation

## Use and maintain the fork

| Guide | Read it for |
|---|---|
| [Usage](USAGE.md) | Generic TCP, UDP, DoH and DoT examples; remote-service model |
| [Configuration](CONFIGURATION.md) | Flag groups, MTU meanings, buffers and starting settings |
| [Architecture](ARCHITECTURE.md) | Source map, data path, ownership and failure handling |
| [Scheduling](SCHEDULING.md) | Optional BBR-lite, polling, fair queues and Go CAKE primitives |
| [Development](DEVELOPMENT.md) | Local builds, formatting, dependencies and Git workflow |
| [Testing](TESTING.md) | Unit, race, integration and simulator coverage |
| [Logging](LOGGING.md) | Event meanings, file rotation and diagnostic limits |
| [Releasing](RELEASING.md) | Stable artifact names, SHA256 and GitHub releases |
| [Upstream](UPSTREAM.md) | Attribution, fork differences and compatibility |
| [Licensing](LICENSING.md) | Retained license texts and third-party source provenance |

[AGENTS.md](../AGENTS.md) points automated contributors to these guides by
task. [CONTRIBUTING.md](../CONTRIBUTING.md) describes patch expectations.

## Reference and history

- [Original upstream documentation](upstream/README.md),
  [CHANGELOG](../CHANGELOG), [TODO](../TODO), and [man pages](../man/README.md).
- [Historical investigations](history/README.md): dated audits, performance
  experiments, fixes, and design proposals, with their original scope limits.
- [Controlled uplink harness](../tests/uplink/README.md) and
  [audit regression fixtures](../tests/audit/20261023/README.md).

The main guides describe current source. Historical notes can mention local
artifact paths that were never part of a public checkout; those are provenance
references, not missing installation dependencies.
