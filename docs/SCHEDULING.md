# Scheduling and flow control

The static path works without either adaptive pacing or fair stream scheduling.
These optional features are separate mechanisms, not requirements for using
the direct TCP carrier. Inspect their flags with `-h` and read
[Configuration](CONFIGURATION.md) for units and endpoint direction.

## Polling and delivery feedback

In [dnstt-client/dns.go](../dnstt-client/dns.go), `DNSPacketConn` converts KCP
packets into queries and decodes TXT replies. Empty polls let the server return
queued downstream data even when the client has no upload payload.
`PollTarget` bounds outstanding empty polls; organic data and ACK/control
queries are not charged as empty polls. Replies provide permits to sustain
the download pipeline; the idle timer supplies backoff polling.

`bbrMonitor` tracks query timestamps, response matching and directional
delivery. [bbr/feedback.go](../bbr/feedback.go) reads KCP headers to separate
acknowledged upload payload from received download payload, avoid counting
retransmissions twice, and measure actual downstream segment sizes. Response
wire sizes and application goodput are different quantities.

## Adaptive client

[bbr/bbr.go](../bbr/bbr.go) contains the bandwidth/RTT model and bounded pacer;
[bbr/adaptive.go](../bbr/adaptive.go) contains the profile hysteresis. The
client's [adaptive.go](../dnstt-client/adaptive.go) validates flags and
translates controller state into KCP windows, poll depth and pacing settings.

With `-bbr`, the client's periodic loop samples both directions, applies
configured segment/byte bounds, and updates the upload pacer. Startup, probe
and drain phases are BBR-inspired; this is not kernel BBR, a complete BBR
implementation, or a claim of fairness with competing TCP flows. ACK/control
and polling progress must remain possible while payload pacing waits.

Controller tests use injected clocks and finite inputs. Updating bandwidth
estimates must not silently discard admitted transmission debt or interpret
an idle handshake as saturated-link capacity. Capacity collapse and recovery
require measurements in each direction, not one shared byte counter.

## Fair queues and CAKE primitives

[fairqueue/fairqueue.go](../fairqueue/fairqueue.go) owns one writer per
scheduled direction. Each flow submits an owned chunk and waits for service.
Sparse/new activity gets priority subject to a burst bound; active bulk flows
share service through deficit round-robin. Per-flow backpressure prevents a
deprioritized source from reading arbitrarily far ahead.

[cake/](../cake) supplies Cobalt state, rate-dependent parameters, and deficit
shaping. Its [package commentary](../cake/doc.go) explains the adaptations from
the retained [Linux reference source](../third_party/cake/README.md). It is a
Go userspace implementation operating on stream chunks, not a Linux qdisc.

The important contracts are:

- Reliable mode waits through queue congestion rather than dropping already
  accepted stream bytes. Failure modes terminate the owning stream explicitly.
- A rate change preserves accumulated serialization debt. A fresh rate is not
  permission to bypass work already admitted under the prior schedule.
- Cancellation releases waiters and flow bookkeeping; one stalled writer must
  not pin unrelated flows indefinitely.
- A clean close must account for accepted tail data before smux FIN.
  [smux_writer.go](../fairqueue/smux_writer.go) supplies the actual writer seam.
- The receive side still has its own smux and KCP budgets. Fair scheduling
  cannot recover latency already trapped behind admitted bytes elsewhere.

The [smux registration patch](../third_party/smux/PATCHES.md) prevents an early
peer reply from being discarded before local registration. Keep its regression
test when upgrading smux even if ordinary transfer tests still pass.

## Where to change and verify

Change estimator math in `bbr`, scheduling mechanics in `fairqueue`/`cake`, and
transport integration in the client/server packages. Test the interface that
actually carries the affected traffic, not only a copied formula. The source
map and lifecycle are in [Architecture](ARCHITECTURE.md); focused commands and
the simulator's limits are in [Testing](TESTING.md).
