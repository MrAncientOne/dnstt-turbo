# Usage

The client exposes a local TCP port; the server connects each tunnel stream to
one configured TCP backend. Run or configure that backend separately. A SOCKS
client works only when the backend is a SOCKS server. SSH and other TCP services
can be forwarded directly. Neither binary configures routing or a TUN device.

Build with `make build` as described in the [README](../README.md). Obtain the
complete current flag list with:

```sh
./build/dnstt-turbo-client -h
./build/dnstt-turbo-server -h
```

## Keypair

Generate once on the server:

```sh
./build/dnstt-turbo-server -gen-key \
  -privkey-file server.key -pubkey-file server.pub
```

The client needs `server.pub`; keep `server.key` on the server. Prefer key
files to putting private material in a command line. The examples below assume
the backend listens on `127.0.0.1:8000`. Key files and local captures/logs are
ignored by source Git.

## Direct TCP

Both endpoints must run this fork for the direct carrier and its private EDNS
upload option. The server must be directly reachable; a normal recursive DNS
resolver is not an interchangeable `-tcp` target for this extension.

Server:

```sh
./build/dnstt-turbo-server -tcp :5300 -mtu 16384 -kcp-window 1024 \
  -privkey-file server.key t.example.com 127.0.0.1:8000
```

Client:

```sh
./build/dnstt-turbo-client -tcp 203.0.113.2:5300 \
  -tcp-upstream-mtu 500 -kcp-window 1024 \
  -pubkey-file server.pub t.example.com 127.0.0.1:7000
```

The domain must match at both endpoints. The address `203.0.113.2` is a
documentation placeholder. Ports below 1024 may require privileges or an
operator-managed forwarding/capability arrangement; port 5300 avoids that in
these examples. Network/firewall/service setup remains an operator choice.

## UDP, DoH and DoT through a resolver

Use a delegated DNS zone you control. For example, the parent zone can contain
an address record for `tns.example.com` pointing to the tunnel server and an NS
record delegating `t.example.com` to that name. The name-server hostname should
sit outside `t.example.com`. Keep the tunnel domain short because QNAME space
is limited. The resolver must reach the authoritative server on UDP port 53;
the example listener below assumes the operator forwards that traffic to 5300.

```sh
./build/dnstt-turbo-server -udp :5300 -mtu 1232 -kcp-window 1024 \
  -privkey-file server.key t.example.com 127.0.0.1:8000
```

Choose one client transport, using your actual recursive resolver:

```sh
./build/dnstt-turbo-client -udp 192.0.2.53:53 \
  -pubkey-file server.pub t.example.com 127.0.0.1:7000

./build/dnstt-turbo-client -doh https://resolver.example/dns-query \
  -pubkey-file server.pub t.example.com 127.0.0.1:7000

./build/dnstt-turbo-client -dot resolver.example:853 \
  -pubkey-file server.pub t.example.com 127.0.0.1:7000
```

These resolver addresses are placeholders, not working public-service
recommendations. For more background on zone delegation and upstream's port
forwarding model, read the [original instructions](upstream/README.md).
DoH/DoT describe the client's link to a resolver, not a TLS listener implemented
by `dnstt-server`.

## Serving UDP and TCP together

Keep independent response budgets:

```sh
./build/dnstt-turbo-server -udp :5300 -tcp :5300 \
  -mtu 1232 -tcp-mtu 16384 -kcp-window 1024 \
  -privkey-file server.key t.example.com 127.0.0.1:8000
```

Each listener owns a separate session space. A client advertising too small a
response cap for that listener's KCP segment is rejected explicitly. Read
[Configuration](CONFIGURATION.md) before changing MTUs or mixing resolver paths.
