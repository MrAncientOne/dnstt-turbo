#!/usr/bin/env python3
"""Build static Linux releases with stable names, source hashes and SHA256SUMS."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
TARGETS = tuple((role, arch) for role in ('client', 'server')
                for arch in ('amd64', 'arm64'))
SOURCE_DIRS = ('dnstt-client', 'dnstt-server', 'dns', 'noise', 'bbr', 'cake',
               'fairqueue', 'diagnostics', 'turbotunnel', 'third_party/smux')


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def source_hashes():
    paths = [ROOT / 'go.mod', ROOT / 'go.sum', ROOT / 'third_party/smux/go.mod']
    for directory in SOURCE_DIRS:
        paths.extend(p for p in (ROOT / directory).glob('*.go')
                     if not p.name.endswith('_test.go'))
    return {str(p.relative_to(ROOT)): digest(p) for p in sorted(paths)}


def git_identity():
    def git(*args):
        result = subprocess.run(['git', *args], cwd=ROOT, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        return result.stdout.strip() if result.returncode == 0 else None
    try:
        return dict(commit=git('rev-parse', 'HEAD'),
                    dirty=bool(git('status', '--porcelain', '--untracked-files=normal')))
    except FileNotFoundError:
        return dict(commit=None, dirty=None)


def validate_output(parser, output, replace):
    # Never replace source or Git metadata, even with --replace.
    if output == ROOT or output in ROOT.parents:
        parser.error('output must not be the source root or its ancestor')
    if ROOT in output.parents and output.relative_to(ROOT).parts[0] not in ('dist', 'build'):
        parser.error('in-repository release output must be inside dist/ or build/')
    if output.is_symlink():
        parser.error('output must not be a symlink')
    if output.exists() and (not output.is_dir() or any(output.iterdir())):
        if not output.is_dir() or not replace or not (output / 'BUILD_INFO.json').is_file():
            parser.error('nonempty output requires --replace and an existing BUILD_INFO.json')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=ROOT / 'dist/latest')
    parser.add_argument('--replace', action='store_true',
                        help='replace a previous release only after all builds succeed')
    args = parser.parse_args()
    # Check the supplied path before resolving it so a symlink cannot hide here.
    if args.output.is_symlink():
        parser.error('output must not be a symlink')
    output = args.output.resolve()
    validate_output(parser, output, args.replace)
    env = os.environ.copy()
    env.pop('GOROOT', None)
    env.update(CGO_ENABLED='0', GOOS='linux')
    go = os.environ.get('GO_BIN') or shutil.which('go')
    if not go:
        parser.error('Go not found; set GO_BIN or put Go on PATH')
    version = subprocess.check_output([go, 'version'], cwd=ROOT, env=env, text=True).strip()
    numbers = re.search(r'go(\d+)\.(\d+)\.(\d+)', version)
    if not numbers or tuple(map(int, numbers.groups())) < (1, 27, 3):
        parser.error('use Go 1.27.3 or newer')
    stamps = [re.search(r'turboBuild\s*=\s*"([^"]+)"',
                        (ROOT / f'dnstt-{role}/main.go').read_text()).group(1)
              for role in ('client', 'server')]
    if stamps[0] != stamps[1]:
        parser.error('client and server build stamps differ')
    sources = source_hashes()
    identity = git_identity()
    flags = ['-trimpath', '-buildvcs=false', '-ldflags=-s -w']
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.' + output.name + '-build-',
                                     dir=output.parent) as tmp:
        stage = Path(tmp) / 'latest'
        stage.mkdir()
        binaries = []
        for role, arch in TARGETS:
            name = f'dnstt-turbo-{role}-linux-{arch}'
            subprocess.run([go, 'build', *flags, '-o', str(stage / name), f'./dnstt-{role}'],
                           cwd=ROOT, env={**env, 'GOARCH': arch}, check=True)
            binaries.append(name)
            print('Built', name, flush=True)
        if sources != source_hashes():
            raise SystemExit('source changed during build; previous release is unchanged')
        notices = {
            'LICENSE': ROOT / 'LICENSE',
            'COPYING': ROOT / 'COPYING',
            'LICENSES.md': ROOT / 'docs/LICENSING.md',
            'SMUX_LICENSE': ROOT / 'third_party/smux/LICENSE',
            'CAKE_REFERENCE.md': ROOT / 'third_party/cake/README.md',
        }
        for name, source in notices.items():
            shutil.copy2(source, stage / name)
        header = (ROOT / 'third_party/cake/sch_cake.c').read_text().split(' * The CAKE Principles:')[0]
        (stage / 'CAKE_ATTRIBUTION').write_text(header + ' */\n')
        notice_names = [*notices, 'CAKE_ATTRIBUTION']
        archives = []
        for name in binaries:
            archive = name + '.tar.gz'
            with tarfile.open(stage / archive, 'w:gz') as tar:
                for item in [name, *notice_names]:
                    tar.add(stage / item, arcname=item)
            archives.append(archive)
        artifacts = [*binaries, *archives, *notice_names]
        info = dict(build=stamps[0], built_utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    compiler=version, CGO_ENABLED=0, GOOS='linux', flags=flags,
                    git=identity, source_hashes=sources,
                    artifacts={name: digest(stage / name) for name in artifacts})
        (stage / 'BUILD_INFO.json').write_text(json.dumps(info, indent=2) + '\n')
        artifacts.append('BUILD_INFO.json')
        (stage / 'SHA256SUMS').write_text(''.join(f'{digest(stage/name)}  {name}\n' for name in artifacts))
        # Keep old output until the complete staging directory is ready. Roll
        # back if installation fails; a compiler failure never touches latest.
        previous = Path(tmp) / 'previous'
        if output.exists():
            output.rename(previous)
        try:
            stage.rename(output)
        except OSError:
            if previous.exists():
                previous.rename(output)
            raise
    print(f'Release ready: {output}', flush=True)


if __name__ == '__main__':
    main()
