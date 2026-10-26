# Logging and diagnostics

Both binaries emit diagnostics to standard error. A service manager can
collect that stream, or a process can additionally append to a rotating file.
Add this option before the positional arguments, using an existing writable
directory:

```sh
-log-file ./client-health.log
```

The file appends on startup and rotates at 10 MiB, keeping three backups.
`-log-max-bytes` changes the rotation threshold. A single oversized log record
can exceed the threshold. The directory must already exist and be writable.
New files use mode 0600; existing file permissions are preserved. Use one log
path per process. The optional file receives the same output as stderr. An
unwritable path fails startup explicitly; runtime file failures still leave
stderr output available. Keep the log path separate from files another process
truncates. SIGKILL cannot produce a final summary.

If you run the server under a systemd service, inspect that service's journal:

```sh
journalctl -u dnstt-server --since '10 minutes ago' --no-pager
```

Journal retention across reboots depends on the host's journald configuration.
A service configured with filesystem restrictions needs an explicitly writable
directory for file logging. Service-unit policy and journal retention are
operator choices, not prerequisites imposed by the tunnel source.

## Reading the events

Events use `role=... event=...` fields alongside existing human-readable logs.
Never infer connectivity from the existing `begin session` line: it precedes
the encrypted handshake. `session_ready` means Noise and smux are initialized,
not that a website or the VPS SOCKS backend is healthy.

| Event | Meaning |
|---|---|
| `session_config` | Effective transport type, MTU/window, buffers and fairness/adaptation flags before buffer clamping |
| `local_listening` | Client local listener exists; handshake may still be pending |
| `tcp_dial_failed` | Actual DNS-over-TCP dial error, previously silently retried |
| `tcp_connected` / `tcp_disconnected` | DNS-over-TCP carrier lifecycle |
| `tcp_read_header_failed` / `tcp_read_body_failed` / `tcp_write_failed` | Framing/I/O failure; errors include EOF and normal shutdown |
| `handshake_started` / `handshake_failed` | Client encrypted handshake progress/failure |
| `session_ready` | Encrypted multiplexed session initialized, with session ID |
| `upstream_dial_failed` | Server could not connect to its configured upstream (usually microsocks) |
| `dns_parse_failed` / `dns_rcode_error` | Invalid DNS response or nonzero numeric RCODE |
| `dns_send_failed` / `dns_control_send_failed` | Query/control emission failed |
| `dns_reply_failed` | Server reply admission/emission failed; inspect the error and cumulative count |
| `adaptive` | Client window, poll and payload pacing/shaper settings, every 30 seconds with BBR enabled |
| `health` | Active carriers/sessions/streams, dial failures, successful framed wire bytes and time since last received frame |
| `failure_count` | Cumulative counts by event, including occurrences suppressed from individual logging |

Health summaries run every 30 seconds while `run` is active and on normal
return. Values are cumulative for the process, not rates or per-session totals.
TCP frame counters exclude the two-byte frame header and count successful
transport operations, not application goodput. Carrier/byte metrics currently
instrument plain DNS-over-TCP; they do not measure UDP/DoH/DoT. Session counters
count initialized smux sessions. Stream/upstream gauges describe activity,
not whether remote websites respond. An idle process can legitimately show no
received traffic. Event counts include expected disconnects and shutdowns;
`failures` is not an internet-availability verdict.

The first occurrence of each new failure type is logged immediately, then at
most once every 30 seconds; every occurrence remains counted. Keys, packets,
SOCKS credentials and complete command lines are not captured. Error strings
can contain endpoint addresses and should be treated as operational metadata.

These logs reduce manual collection but cannot observe hotspot firewall,
routes, Wi-Fi or tun2socks internals. If SOCKS probes succeed while hotspot
clients fail, host-level diagnostics can still be necessary. No automatic
internet probes or additional network traffic are introduced.
