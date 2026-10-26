#!/usr/bin/env python3
"""Exercise packaging and replacement failure paths without a real compiler."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
FAKE_GO = '''#!/usr/bin/env python3
import os, pathlib, sys
if sys.argv[1] == 'version':
    print('go version go1.27.3 linux/amd64')
elif sys.argv[1] == 'build':
    if os.environ.get('FAIL_BUILD') == os.environ['GOARCH']:
        sys.exit(1)
    p = pathlib.Path(sys.argv[sys.argv.index('-o') + 1])
    p.write_bytes((sys.argv[-1] + os.environ['GOARCH']).encode())
    p.chmod(0o755)
else:
    sys.exit(2)
'''


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='dnstt-release-test-')
        self.addCleanup(self.tmp.cleanup)
        self.parent = Path(self.tmp.name)
        self.output = self.parent / 'latest'
        compiler = self.parent / 'go'
        compiler.write_text(FAKE_GO)
        compiler.chmod(0o755)
        self.env = dict(os.environ, GO_BIN=str(compiler), PYTHONDONTWRITEBYTECODE='1')

    def build(self, *args, **env):
        return subprocess.run([sys.executable, str(ROOT / 'scripts/build-release.py'),
                               '--output', str(self.output), *args],
                              env=dict(self.env, **env), text=True, capture_output=True)

    def test_packages_and_checksums(self):
        result = self.build()
        self.assertEqual(result.returncode, 0, result.stderr)
        for line in (self.output / 'SHA256SUMS').read_text().splitlines():
            expected, name = line.split('  ')
            self.assertEqual(hashlib.sha256((self.output / name).read_bytes()).hexdigest(), expected)
        info = json.loads((self.output / 'BUILD_INFO.json').read_text())
        self.assertIn('third_party/smux/go.mod', info['source_hashes'])
        for role in ('client', 'server'):
            for arch in ('amd64', 'arm64'):
                name = f'dnstt-turbo-{role}-linux-{arch}'
                with tarfile.open(self.output / (name + '.tar.gz')) as tar:
                    self.assertIn(name, tar.getnames())
                    self.assertIn('SMUX_LICENSE', tar.getnames())
                    self.assertIn('CAKE_ATTRIBUTION', tar.getnames())

    def test_failed_rebuild_preserves_previous_output(self):
        self.assertEqual(self.build().returncode, 0)
        original = (self.output / 'SHA256SUMS').read_bytes()
        result = self.build('--replace', FAIL_BUILD='arm64')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.output / 'SHA256SUMS').read_bytes(), original)
        self.assertEqual(list(self.parent.glob('.latest-build-*')), [])
        self.assertEqual(self.build('--replace').returncode, 0)

    def test_requires_explicit_replace_and_preserves_unrelated_directory(self):
        self.output.mkdir()
        keep = self.output / 'keep.txt'
        keep.write_text('operator data')
        self.assertNotEqual(self.build('--replace').returncode, 0)
        self.assertEqual(keep.read_text(), 'operator data')
        keep.unlink()
        self.assertEqual(self.build().returncode, 0)
        self.assertNotEqual(self.build().returncode, 0)

    def test_refuses_symlink_destination(self):
        real = self.parent / 'real'
        real.mkdir()
        self.output.symlink_to(real, target_is_directory=True)
        self.assertNotEqual(self.build('--replace').returncode, 0)


if __name__ == '__main__':
    unittest.main()
