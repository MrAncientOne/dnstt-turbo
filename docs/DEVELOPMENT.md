# Development

## Requirements and local builds

Use Go 1.27.3 or newer and Python 3.9 or newer for repository tooling. GNU Make
is optional. The race detector needs a supported platform and C compiler;
release binaries are built with `CGO_ENABLED=0`.

```sh
go mod download
make build
make check
make race
```

Without Make:

```sh
mkdir -p build
go build -trimpath -buildvcs=false -o build/dnstt-turbo-client ./dnstt-client
go build -trimpath -buildvcs=false -o build/dnstt-turbo-server ./dnstt-server
go test ./... -count=1 -timeout=180s
go vet ./...
```

Test the nested dependency separately: from `third_party/smux`, run
`go test ./...` and `go test -race ./...`. Its own module excludes it from root
`./...`; `make test` and `make race` include both modules automatically.

`GO=/absolute/path/to/go` selects a compiler for Make, and `GO_BIN` selects it
for Python build/simulation tools. `GOMODCACHE` and `GOCACHE` can select caches.
Unset a stale externally configured `GOROOT` when selecting a different Go
installation. The release builder does that for its own compiler subprocesses.

## Layout and dependencies

The client/server and core packages keep upstream's layout. Additional tunnel
code lives in `bbr`, `cake`, `fairqueue`, and `diagnostics`. The
[architecture guide](ARCHITECTURE.md) links each responsibility to source.
Tests live beside the packages they exercise; reusable controlled integration
tests are under `tests/`. Generated simulator modules live under ignored
`build/` paths.

`go.mod` retains `www.bamsoftware.com/git/dnstt.git` for internal imports.
Building a local clone with `go build ./dnstt-client` uses that clone's source.
A module-name migration would need a separate deliberate change, including
imports and harness seams; changing the GitHub repository name does not require
it. The module has a local replacement for the patched smux dependency.
Retain and test that patch when updating dependencies.

## Editing and validation

- Format changed Go code with `gofmt` or `make fmt`. `make fmt-check` checks
  root-module Go source without rewriting the vendored dependency.
- Keep source changes local to the owning package. Repository cleanup does
  not justify modifying timing, queue limits, defaults or packet framing.
- Add regression tests at the real failing seam. See [Testing](TESTING.md).
- Update the owning guide and run `make docs-check` when paths or commands
  change. The checker validates relative Markdown file links offline.
- Keep test payloads synthetic and examples generic. Credentials, production
  host addresses, routing rules and service units belong to deployment tooling.

## Git and publication

Keep the canonical dnstt history described in [Upstream](UPSTREAM.md). The
`upstream` remote points to `https://www.bamsoftware.com/git/dnstt.git`;
`origin` points to the maintainer's GitHub repository. Inspect `git remote -v`
before fetching or publishing. Avoid reinitializing the repository or replacing
its ancestry with a third-party mirror.

To publish another copy of this fork, create an empty GitHub repository in
your own account without adding a README, license, or initial commit. Set
`origin` to that new repository's clone URL and push `main`. Canonical upstream
is outside GitHub, so a third-party GitHub mirror's Fork button does not create
a direct GitHub fork of bamsoftware. There is no need to change the retained
Go module path to publish a local clone under a different GitHub name.

Before committing, inspect `git status --short`, `git diff`, and
`git diff --cached`. Generated `build/`, `dist/`, old `bin/`, keys, captures,
and logs are ignored. The local smux source and all package regression tests
must be included in the commit. Keep the retained upstream license/provenance
files. The [release workflow](RELEASING.md) attaches built binaries separately
instead of committing executable blobs into source history.
