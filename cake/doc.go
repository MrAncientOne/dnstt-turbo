// Package cake is a userspace port of the core CAKE mechanisms
// (Cobalt AQM, DRR++ deficit scheduling, deficit-mode shaper, rate
// auto-tuning) for pacing tunnel streams into one smux session.
//
// Origin: Linux kernel net/sched/sch_cake.c (GPL-2.0 OR BSD-3-Clause;
// Jonathan Morton, Toke Høiland-Jørgensen, Dave Täht, Sebastian Moeller,
// Kevin Darbyshire-Bryant, Ryan Mounce). The C source is vendored
// read-only at ../../third_party/cake/sch_cake.c and is NOT compiled.
// This package reimplements its packet-path logic in Go; function names
// and section comments cite the C originals (e.g. "sch_cake.c:cobalt_should_drop").
//
// Model mapping (kernel -> tunnel), load-bearing:
//   - skb/packet      -> one stream chunk ([]byte, <= Chunk bytes). The
//     scheduler never fragments; DRR deficit may go negative exactly as
//     in cake_dequeue_one/cake_dequeue.
//   - flow hash table -> explicit flows. The tunnel already knows its
//     streams (one Flow per smux stream), so there is no 8-way
//     set-associative hash, no collisions, no triple-isolate host tables.
//     cake_get_flow_quantum reduces to flowQuantum (+- dither).
//   - Diffserv tins   -> single BESTEFFORT tin (cake_config_besteffort).
//     Sparse-vs-bulk priority comes from CAKE's new/old/decaying lists
//     within the tin, with the tunnel's proven time-based sparse detector
//     (idle gap) deciding new-vs-old entry, because blocking Submit keeps
//     per-flow depth at 1 (a queue that always drains would otherwise
//     re-enter every flow as SPARSE and defeat DRR; see Sched.Submit).
//   - Drops           -> NEVER silent. A CoDel/BLUE or overlimit drop
//     decision fails the owning Submit with ErrDrop (caller tears the
//     stream down; end-to-end TCP retransmits). This is the analogue of
//     qdisc packet drop, and the reason bulk-vs-bulk stays fair while no
//     stream bytes are ever lost in-RAM.
//   - ECN             -> counted (ecnMarked) but treated as drop: smux
//     offers no ECN wire channel.
//   - Time            -> time.Time (monotonic); rate_ns/rate_shft fixed-point
//     shaper arithmetic is preserved verbatim (cake_set_rate,
//     cake_advance_shaper) so the deficit-mode (no burst parameter)
//     behavior matches.
//   - Linger          -> tunnel-specific (smux FIN jumps queued data); kept
//     in the fairqueue wrapper, not here.
//
// What is deliberately NOT ported: IP/TCP header parsing and flow hashing,
// DSCP/wash, NAT/conntrack, GSO splitting, ACK filtering, ingress mode,
// autorate-ingress estimator (BBR already estimates; feed it via SetRate),
// multi-queue (cake_mq) sharing, netlink config/dump. The shaper rate is
// set explicitly via SetRate (BBR SustainedBW); interval/target auto-tune
// from it per cake_set_rate.
package cake
