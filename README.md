# dnstt-turbo

A fork of [dnstt](https://www.bamsoftware.com/software/dnstt/), David Fifield's
userspace DNS tunnel. It retains the upstream client/server package layout
and adds a direct DNS-over-TCP carrier, optional adaptive pacing and fair
stream scheduling, bounded queues, recovery handling, and diagnostics.
Git ancestry starts at the canonical bamsoftware repository's
`v1.20260501.0` release; [Upstream relationship](docs/UPSTREAM.md) records the
exact commit and explains the GitHub publication model.

Like upstream dnstt, it forwards a local TCP port to a TCP service reachable
from the server. The tunnel encrypts application traffic and authenticates
the server using its public key. It does not create a TUN interface or provide
a SOCKS server; a SOCKS proxy can be the remote TCP service.

This repository contains the tunnel source, tests, and build tooling. Routing,
hotspot setup, firewall rules, tun2socks, Linux qdiscs, and server-specific
deployment scripts belong to the operator's separate deployment.

## Build

Use Go **1.27.3 or newer** for maintained builds. The pinned toolchain is in
[go.mod](go.mod); keep the local [smux patch](third_party/smux/PATCHES.md).

```sh
go mod download
make build
```

This creates `build/dnstt-turbo-client` and `build/dnstt-turbo-server` for your
current platform. Make is a convenience; the equivalent Go commands are in
[Development](docs/DEVELOPMENT.md).

For static Linux client and server binaries on amd64 and arm64:

```sh
make release
cd dist/latest
sha256sum -c SHA256SUMS
```

Binary filenames stay the same across builds. Identify a build by its
SHA256 and `BUILD_INFO.json`, rather than adding incident names to filenames.
[Releases](docs/RELEASING.md) explains local artifacts and the optional GitHub
Actions workflow for uploading binaries to a `latest` release.

## Direct DNS-over-TCP example

First run a TCP service on the server at `127.0.0.1:8000`. That might be an
application, an SSH daemon on another port, or a separately managed SOCKS proxy.
Generate the tunnel keypair on the server:

```sh
./build/dnstt-turbo-server -gen-key \
  -privkey-file server.key -pubkey-file server.pub
```

Keep `server.key` on the server and copy `server.pub` to the client. On the
server, start the tunnel on unprivileged TCP port 5300:

```sh
./build/dnstt-turbo-server \
  -tcp :5300 -mtu 16384 -kcp-window 1024 \
  -privkey-file server.key t.example.com 127.0.0.1:8000
```

On the client, substitute your server's address:

```sh
./build/dnstt-turbo-client \
  -tcp 203.0.113.2:5300 -tcp-upstream-mtu 500 -kcp-window 1024 \
  -pubkey-file server.pub t.example.com 127.0.0.1:7000
```

Connect your application to `127.0.0.1:7000`; its bytes reach the configured
server-side service. For example, use SOCKS settings only if that remote
service actually speaks SOCKS. Both endpoints must use the same tunnel domain.
Direct TCP requires network reachability to the server; its domain is a
protocol identifier, not a requirement to route through a recursive resolver.
Use a domain you control for a real deployment.

These are explicit starting settings, not universal optimal values or the
complete program defaults. UDP, DoH, and DoT use the resolver/delegated-zone
model described in [Usage](docs/USAGE.md). The TCP carrier and EDNS upload
extension require this fork at both ends. See
[Configuration](docs/CONFIGURATION.md) for transport and MTU constraints.

## Tests and documentation

```sh
make check       # formatting, documentation links, tooling tests, Go tests/vet
make race        # root module and patched smux tests with the race detector
```

- [Documentation index](docs/README.md): guides and source map.
- [Architecture](docs/ARCHITECTURE.md): packet flow, ownership, queues, recovery.
- [Development](docs/DEVELOPMENT.md): editing and validation workflow.
- [Testing](docs/TESTING.md): regression tests and controlled uplink simulation.
- [Upstream relationship](docs/UPSTREAM.md): fork differences and compatibility.
- [Contributing](CONTRIBUTING.md) and [agent instructions](AGENTS.md).

Tests establish behavior for their workloads; simulator speeds are not WAN
capacity promises. Historical investigations are kept separately in
[docs/history](docs/history/README.md), and are not current setup instructions.

## Attribution and license files

Credit for the original dnstt design and implementation belongs to David
Fifield and upstream contributors. Original documentation is retained in
[docs/upstream](docs/upstream/README.md), with the upstream [CHANGELOG](CHANGELOG)
and [man pages](man/README.md) available for reference.

The existing [LICENSE](LICENSE), upstream [COPYING](COPYING), and third-party
notices are retained. See [Attribution and license inventory](docs/LICENSING.md)
for the provenance and license files of included code.
