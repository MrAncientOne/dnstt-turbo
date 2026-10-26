#!/usr/bin/env python3
"""Inject isolated audit tests with Go overlays; never modify production files.

These tests intentionally fail on the audited source. Exit status1 means a
regression assertion failed; inspect the saved Go output for the exact finding.
Normal `go test ./...` does not compile the .go.txt fixtures in this directory.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[2]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--group', choices=['all', 'server', 'shaper', 'small-segment', 'official'], default='all')
    parser.add_argument('--official', type=Path, help='unpacked official release directory; required for group official')
    parser.add_argument('--race', action='store_true')
    parser.add_argument('--repeat', type=int, default=3)
    parser.add_argument('--output', type=Path, help='retain overlay and Go logs in this directory')
    args = parser.parse_args()
    if args.repeat < 1:
        parser.error('repeat must be positive')
    if args.group == 'official' and args.official is None:
        parser.error('--official is required for group official')
    go = os.environ.get('GO_BIN') or shutil.which('go') or '/usr/local/go/bin/go'
    groups = {
        'server': ('dnstt-server', 'server_test.go.txt',
                   '^TestAudit(UnauthenticatedClientIDAllocation|SlowTCPReaderDoesNotBlockOtherClient|AcceptedSmallResponseCapCanCarryConfiguredSegment)$'),
        'shaper': ('fairqueue', 'shaper_test.go.txt', '^TestAudit(SetRate|LiveRate)'),
        'small-segment': ('dnstt-client', 'small_segment_test.go.txt', '^TestAuditSmallDownstreamSegmentsSizeActualFlight$'),
        'official': ('dnstt-server', 'official_test.go.txt', '^TestAudit'),
    }
    selected = ['server', 'shaper', 'small-segment'] if args.group == 'all' else [args.group]
    output = args.output.resolve() if args.output else Path(tempfile.mkdtemp(prefix='dnstt-audit-20261023-'))
    output.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env.setdefault('GOPROXY', 'off')
    results = []
    for group in selected:
        package, fixture, pattern = groups[group]
        root = args.official.resolve() if group == 'official' else REPO
        source = output / (group + '_test.go')
        source.write_bytes((HERE / fixture).read_bytes())
        overlay = output / (group + '-overlay.json')
        overlay.write_text(json.dumps({'Replace': {str(root / package / 'zz_audit_20261023_test.go'): str(source)}}))
        # The fixes promote the assertions into their real packages. Run those
        # versions (including explicit MTU rejection) without duplicate symbols;
        # keep the immutable .go.txt probes for auditing old source snapshots.
        promoted = root / package / 'audit_regression_test.go'
        overlay_args = [] if group != 'official' and promoted.exists() else ['-overlay=' + str(overlay)]
        command = [go, 'test', *overlay_args, './' + package,
                   '-run', pattern, '-count=' + str(args.repeat), '-v', '-timeout=30s']
        if args.race:
            command.insert(2, '-race')
        log = output / (group + '.log')
        with log.open('w') as stream:
            result = subprocess.run(command, cwd=root, env=env, stdout=stream, stderr=subprocess.STDOUT)
        print(log.read_text(), end='')
        results.append({'group': group, 'command': command, 'exit_code': result.returncode, 'log': str(log)})
    (output / 'results.json').write_text(json.dumps(results, indent=2) + '\n')
    print('Evidence:', output)
    raise SystemExit(0 if all(r['exit_code'] == 0 for r in results) else 1)


if __name__ == '__main__':
    main()
