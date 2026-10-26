# Vendored CAKE reference source

Origin: Linux kernel, `net/sched/sch_cake.c`
Fetched: 2026-09-24 from
https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/plain/net/sched/sch_cake.c
(kernel HEAD at fetch time: `62f4c998b297cf233997a2b4cd6fc2d2df0319c9`)

This file is READ-ONLY reference. It is NOT compiled into dnstt-turbo.
The Go implementation in [cake](../../cake/) is a userspace adaptation of the
core CAKE mechanisms (Cobalt AQM, DRR++ deficit, deficit-mode shaper,
rate auto-tuning); see [cake/doc.go](../../cake/doc.go) for the exact mapping and
adaptations for the smux-chunk (not skb) model.

License: `sch_cake.c` header is `SPDX-License-Identifier: GPL-2.0 OR
BSD-3-Clause` with copyrights for Jonathan Morton, Toke
Høiland-Jørgensen, Dave Täht, Sebastian Moeller, Kevin
Darbyshire-Bryant, Ryan Mounce. These source notices are retained unchanged.
See the [license inventory](../../docs/LICENSING.md), the existing
[root LICENSE](../../LICENSE), and [COPYING](../../COPYING) for the retained
texts; the root LICENSE contains GPL version 3, not GPL version 2.
