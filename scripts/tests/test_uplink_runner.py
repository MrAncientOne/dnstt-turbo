#!/usr/bin/env python3
"""Exercise the uplink CLI's compiler selection without running simulations."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
FAKE_GO = '''import pathlib, sys
if sys.argv[1] == 'version':
    print('go version go1.27.3 runner-fixture')
elif sys.argv[1:3] == ['test', '-c']:
    binary = pathlib.Path(sys.argv[sys.argv.index('-o') + 1])
    binary.write_text('#!' + sys.executable + '\\nprint("runner fixture executed")\\n')
    binary.chmod(0o755)
else:
    sys.exit(2)
'''


class UplinkRunnerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='dnstt-runner-test-')
        self.addCleanup(self.tmp.cleanup)
        self.parent = Path(self.tmp.name)
        compiler_dir = self.parent / 'bin'
        compiler_dir.mkdir()
        self.compiler = compiler_dir / 'go'
        self.compiler.write_text('#!' + sys.executable + '\n' + FAKE_GO)
        self.compiler.chmod(0o755)
        self.output = self.parent / 'results'
        self.env = dict(os.environ, PATH=str(compiler_dir),
                        PYTHONDONTWRITEBYTECODE='1')
        self.env.pop('GO_BIN', None)

    def run_cli(self, compiler=None):
        env = self.env.copy()
        if compiler is not None:
            env['GO_BIN'] = compiler
        return subprocess.run(
            [sys.executable, str(ROOT / 'tests/uplink/run.py'),
             '--mode', 'performance', '--protocol-only',
             '--case', 'regression-static', '--output', str(self.output)],
            cwd=self.parent, env=env, text=True, capture_output=True,
            timeout=15)

    def assert_compiler_used(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        metadata = json.loads((self.output / 'environment.json').read_text())
        self.assertEqual(metadata['go'], 'go version go1.27.3 runner-fixture')
        results = json.loads((self.output / 'results.json').read_text())
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0]['exit'], 0)
        self.assertIn('runner fixture executed',
                      (self.output / 'performance-regression-static-1.log').read_text())

    def test_command_name_on_path(self):
        self.assert_compiler_used(self.run_cli('go'))

    def test_absolute_path(self):
        self.assert_compiler_used(self.run_cli(str(self.compiler)))

    def test_relative_path_survives_module_directory_change(self):
        self.assert_compiler_used(self.run_cli('./bin/go'))

    def test_unset_go_bin_uses_path(self):
        self.assert_compiler_used(self.run_cli())

    def test_invalid_override_does_not_fall_back_to_path(self):
        result = self.run_cli(str(self.parent / 'missing-go'))
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertIn('Go toolchain missing', result.stderr)
        self.assertFalse(self.output.exists())


if __name__ == '__main__':
    unittest.main()
