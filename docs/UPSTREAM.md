# Upstream relationship

dnstt-turbo is a fork of David Fifield's
[dnstt](https://www.bamsoftware.com/software/dnstt/). Its branch descends from
the canonical repository at
[`https://www.bamsoftware.com/git/dnstt.git`](https://www.bamsoftware.com/git/dnstt.git).

The base is official release **`v1.20260501.0`**, commit
`0c5c52a57d899c05428c116898941761a2ed83c2`, which was also the release used by
the earlier controlled upstream comparison. The exact base is recorded in
[UPSTREAM_BASE.json](UPSTREAM_BASE.json). The tested turbo source was applied
onto this base without changing any Go file or module dependency declaration.

An earlier working checkout came through the third-party GitHub mirror
`gh4rib/dnstt`. That mirror is not official upstream and is not the parent of
this branch. The old local history was preserved separately during migration;
this public branch retains the canonical dnstt ancestry instead.

Remote roles are:

- `upstream`: the canonical bamsoftware repository, for upstream comparisons.
- `origin`: the maintainer's GitHub repository, for publishing this fork.

Because canonical upstream is hosted outside GitHub, publish a new empty
GitHub repository rather than using a third-party mirror's Fork button. The
project is a source-code fork even without GitHub's fork-network badge.

The original [README](upstream/README.md), [plain README](upstream/README),
[CHANGELOG](../CHANGELOG), [TODO](../TODO), [COPYING](../COPYING), and
[manual pages](../man/README.md) are retained. They describe upstream behavior
at their source snapshot, not all added flags in this fork.

## Kept and extended

| Area | Relationship to upstream |
|---|---|
| Layout | Retains client/server executables and the core DNS, Noise and turbotunnel packages |
| Basic model | Local TCP forwarding to one server-side backend through encrypted KCP/smux streams |
| Resolver transports | Retains UDP, DoH and DoT client paths |
| Direct carrier | Adds DNS-over-TCP client/server framing and a private EDNS upload option |
| Flow control | Adds optional BBR-inspired client control, polling knobs and fair stream scheduling |
| Resource/lifetime handling | Adds bounded peer/reply queues, admission checks, failure propagation and fresh-session recovery |
| Diagnostics | Adds structured health/counter events and optional rotating files |
| smux dependency | Retains wire version 2 with a minimal local registration-order fix |

## Compatibility boundaries

Direct `-tcp` and nonzero `-tcp-upstream-mtu` need this fork at both ends.
Resolver transports retain the upstream DNS tunnel model, but this fork's
changed defaults, response budgets and admission checks mean arbitrary
upstream/fork flag combinations are not a blanket interoperability guarantee.
Test the specific versions and settings you intend to pair.

Keep UDP and TCP response limits separate on mixed listeners. The server
rejects queries whose advertised response cap cannot carry the listener's
whole configured segment; explicit rejection is preferable to silently
truncating encrypted data. See [Configuration](CONFIGURATION.md).

The Go module path remains `www.bamsoftware.com/git/dnstt.git` for internal
imports and test seams. Local clone builds use local source; the module path
does not mean the published binaries are official upstream releases.

## Comparing upstream

Use a fixed upstream release/commit when auditing drift, and record the exact
revision and downloaded source hash. The previous controlled comparison's
[upstream metadata](../tests/audit/20261023/upstream.json) names the snapshot
used then; it is not a claim that the same version remains the newest upstream.
The [historical audit](history/AUDIT_2026-10-23.md) records its limitations.

Retain license and attribution files during merges and compare the
[local smux patch](../third_party/smux/PATCHES.md) against an updated dependency
before removing the replacement. Keep runtime refactoring separate from a
repository/documentation cleanup so behavior changes remain reviewable.
