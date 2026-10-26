# Attribution and retained license files

This cleanup preserves the license texts already present in the fork. It does
not change the license of upstream files or third-party dependencies.

| Material | Provenance / retained text |
|---|---|
| Fork's existing root license | [LICENSE](../LICENSE) contains GNU GPL version 3 |
| Original dnstt | David Fifield and upstream contributors; public-domain dedication and [CC0 text in COPYING](../COPYING) |
| Vendored smux | `github.com/xtaci/smux v1.5.24`; [MIT license](../third_party/smux/LICENSE), [local patch](../third_party/smux/PATCHES.md) |
| CAKE reference | Linux `net/sched/sch_cake.c`, pinned provenance in [VERSION](../third_party/cake/VERSION); header declares `GPL-2.0 OR BSD-3-Clause` |
| Go CAKE adaptation | Origin and changes described in [cake/doc.go](../cake/doc.go); original contributor attribution is retained there and in the reference source |
| Other Go dependencies | Versions and checksums in [go.mod](../go.mod) and [go.sum](../go.sum); their own license notices remain with their source distributions |

Canonical dnstt carries its public-domain/CC0 dedication in `COPYING`; it does
not supply this fork's root GPLv3 `LICENSE`. That file was inherited from the
previous turbo checkout and is retained without rewriting its text. Restoring
canonical Git ancestry does not make the mirror's license file an official
upstream license declaration.

The vendored C CAKE source is a read-only reference; it is not compiled into
the Go binary. Its [README](../third_party/cake/README.md) describes that scope.
The historical note that called the root `LICENSE` GPL-2.0 was inconsistent
with the actual retained GPLv3 file and has been corrected to an inventory of
the source notices rather than making a new licensing choice.

The release builder includes the root texts, this inventory, the smux MIT
notice, and the CAKE reference README/header attribution in each binary archive.
Source distributions retain the complete reference and patched dependency.
Future dependency additions or licensing changes should update this inventory
alongside their own notices.
