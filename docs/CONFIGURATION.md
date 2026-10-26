# Configuration

`-h` on each compiled binary is the authoritative list of flags and defaults.
This guide explains relationships and units rather than duplicating every
flag. Pass flags before positional arguments.

```text
client: [transport and tuning flags] -pubkey-file FILE DOMAIN LOCALADDR
server: [listener and tuning flags] -privkey-file FILE DOMAIN UPSTREAM
```

`LOCALADDR` is the client's local TCP listener; `UPSTREAM` is the TCP backend
reachable from the server. These addresses need not be the same. See
[Usage](USAGE.md) for examples.

## Transports and identity

| Setting | Meaning |
|---|---|
| Client `-udp`, `-doh`, `-dot` | Client-to-recursive-resolver transport; authoritative side uses UDP |
| Client `-tcp` | Direct DNS-over-TCP to this fork's server |
| Server `-udp`, `-tcp` | Listener addresses; either or both can be configured |
| `-pubkey-file`, `-privkey-file` | Pin the server public key / load its private key |
| Client `-utls` | DoH/DoT TLS fingerprint selection; does not affect a plain `-tcp` carrier |

Client and server must agree on domain and server identity. The Noise
handshake authenticates the server; local-listener binding and backend access
control remain separate concerns. Bind local-only services to loopback when
that is the intended exposure.

## Three different size limits

1. **DNS response budget:** server `-mtu` bounds DNS replies. `-tcp-mtu`
   optionally sets a separate TCP reply budget. These are wire-message caps,
   not interface MTUs or KCP segment sizes. Mixed listeners commonly use
   `-mtu 1232 -tcp-mtu 16384`.
2. **Downstream KCP segment cap:** server `-kcp-mtu-cap` limits one segment.
   A larger DNS response bundles several segments rather than making KCP
   accept an oversized MTU. A listener rejects advertised caps that cannot
   carry its whole configured segment with DNS overhead.
3. **Upstream packet size:** client `-tcp-upstream-mtu` selects private EDNS
   upload encoding for direct TCP. Zero uses the QNAME-bound encoding. A
   nonzero value is validated in `80..1500`; usable values depend on the
   actual path. A setting of 500 is a tested example, not a universal optimum.

Increasing a response or EDNS size changes path compatibility and overhead.
Keep the same sizes during a reliability or pacing comparison. UDP/recursive
paths need their own measurements; direct TCP settings are not portable to
arbitrary resolvers. The server also applies the client's advertised response
cap when assembling replies and bounded error messages.

## Windows, polls and buffers

| Flag | Unit / purpose |
|---|---|
| Both `-kcp-window` | Segments in KCP send/receive windows; wire limit 65535 |
| Client `-poll-limit` | Baseline outstanding empty polls; more polls help only if polling is limiting delivery |
| Client `-poll-burst` | Poll permits added per data reply; distinct from the outstanding target |
| Client `-init-poll-delay`, `-max-poll-delay` | Idle polling/backoff intervals |
| Both `-stream-buffer` | smux bytes buffered for one receiving stream |
| Both `-smux-recvbuf` | Session-wide receiving budget; clamped to at least the per-stream budget |
| Both `-tcp-sockbuf` | Optional kernel buffers on the outer TCP carrier; zero keeps OS defaults |
| Both `-kcp-acknodelay` | Immediate KCP ACK flushing; more control traffic is not automatically faster |
| Client `-max-streams` | Admission cap for local streams; zero is unlimited |

Flags are directional: receiver buffers matter on the receiving endpoint;
the server send window constrains downloads even if the client is adaptive.
Packet queues are sized at startup from the configured windows and resource
budgets. Windows and buffers are not bandwidth targets, and making them larger
can increase outstanding work, memory use and delay.

The command-line defaults and the chosen deployment profile are different.
For example, an operator may explicitly choose window 1024 and large smux
buffers while retaining static pacing. Record the complete effective settings
when comparing builds. [Logging](LOGGING.md) explains what `session_config`
includes and omits.

## Optional scheduling

- Client `-bbr` enables the fork's BBR-inspired estimates and upload pacer;
  it is independent of the host kernel's TCP congestion-control choice.
- `-bbr-min-window`, `-bbr-max-window`, `-bbr-max-flight`, and `-bbr-min-rate`
  bound adaptive behavior. Read [Scheduling](SCHEDULING.md) before tuning them.
- Client `-fair-upload` and server `-fair-download` enable the corresponding
  per-stream scheduler. The Go `cake` implementation is used internally;
  enabling it does not install Linux CAKE or configure a TUN interface.
- `-fair-submit-timeout 0` selects reliable backpressure through congestion.
  A positive starvation timeout changes failure behavior, so it is not an
  equivalent throughput optimization.

## Logs and tuning procedure

Both binaries accept `-log-file` and `-log-max-bytes`; the destination directory
must exist. Standard error remains available. See [Logging](LOGGING.md).

Keep a known successful binary/settings pair. Change one variable, use the
same byte counts and endpoint, compare repeated runs, and check completion,
session continuity and interactive delay. Invalid direct baselines prevent
efficiency comparisons but do not erase successful tunnel transfers. For
maintainer-controlled experiments, use the [test harness](TESTING.md).
