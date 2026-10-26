# Builds and GitHub releases

Keep source Git free of executables. Build artifacts use stable filenames in
`latest`; SHA256 and the build manifest distinguish the actual bytes. The
historical `turboBuild` string is a diagnostic label, not an artifact name or a
complete source identity. Do not change tested code merely to rename a release.

## Local builds

For your own platform:

```sh
make build
```

For static Linux client/server binaries on amd64 and arm64:

```sh
make release
cd dist/latest
sha256sum -c SHA256SUMS
```

`make release` calls [scripts/build-release.py](../scripts/build-release.py).
It requires Go 1.27.3 or newer, builds with CGO disabled, strips debug symbols,
and embeds no working-directory path or automatic VCS stamp in the binaries.
The manifest records the compiler, flags, source hashes, Git commit and dirty
state. Building from a source archive works without Git metadata.

Artifacts include:

- `dnstt-turbo-client-linux-amd64` and `dnstt-turbo-client-linux-arm64`.
- `dnstt-turbo-server-linux-amd64` and `dnstt-turbo-server-linux-arm64`.
- A matching `.tar.gz` per binary, including license/attribution notices.
- `BUILD_INFO.json` and `SHA256SUMS`, plus standalone notice files.

The builder stages all outputs before replacing `dist/latest`. Compilation or
source-change failures leave the previous release untouched. Custom paths are
supported with `--output DIR`; replacing an existing release requires
`--replace`. A nonempty unrelated directory is rejected. This is a local build
operation: it does not upload, install, restart or deploy anything.

On macOS, `shasum -a 256` can inspect an individual binary. Source hashes and
binary hashes answer different questions: compiler/flags/dependencies also
affect output. Archive metadata and build timestamps can vary between builds
even when executable bytes match.

## GitHub Actions

After committing and pushing this source to your own GitHub repository:

1. Open **Actions → Build latest binaries → Run workflow** on the default
   branch.
2. Leave **publish** unchecked to build/test and download the run's `latest`
   artifact without publishing a release.
3. Check **publish** to upload or replace the repository's release assets under
   the tag/title `latest`. The workflow deliberately moves the `latest` tag to
   that run's commit and replaces same-named assets; it never changes a branch.

The [workflow](../.github/workflows/release.yml) runs normal and race tests,
builds all four targets, and verifies checksums before upload. The explicit
publishing steps use the built-in repository token; no additional personal
access token is required. It runs from the default branch, serializes
release operations, and uses pinned action commits. Repository rules that
protect `latest` or enforce immutable releases must permit this deliberately
mutable channel; otherwise use a separately designed versioned-release policy.

Download links have the stable form below, replacing the owner/repository:

```text
https://github.com/OWNER/REPOSITORY/releases/download/latest/dnstt-turbo-server-linux-arm64
https://github.com/OWNER/REPOSITORY/releases/download/latest/SHA256SUMS
```

The normal [CI workflow](../.github/workflows/ci.yml) validates pushes and pull
requests with read-only repository permissions. Neither workflow is executed
by merely having these files locally. No GitHub publication is performed by
repository cleanup.

## Manual upload

You can also create a release in GitHub's Releases page and attach the files
from `dist/latest`. Use title/tag `latest` if you want the stable links above.
With an already authenticated GitHub CLI and an existing `latest` release,
this uploads the local assets:

```sh
gh release upload latest dist/latest/* --clobber
```

That command replaces existing same-named assets. Keep the release's source
tag/notes consistent with `BUILD_INFO.json`; the provided workflow handles
both together. See the official
[GitHub CLI upload reference](https://cli.github.com/manual/gh_release_upload)
and [setup-go toolchain reference](https://github.com/actions/setup-go).
