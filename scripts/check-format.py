#!/usr/bin/env python3
"""Check root-module Go formatting without changing source or vendored files."""
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
go = os.environ.get('GO', 'go')
gofmt = shutil.which('gofmt')
if Path(go).is_absolute() and Path(go).with_name('gofmt').is_file():
    gofmt = str(Path(go).with_name('gofmt'))
if not gofmt:
    raise SystemExit('gofmt not found; install Go or put it on PATH')
paths = sorted(p for directory in ROOT.iterdir()
               if directory.is_dir() and directory.name not in
               {'.git', 'third_party', 'build', 'dist', 'tests', 'scripts', 'docs', '.github'}
               for p in directory.rglob('*.go'))
result = subprocess.run([gofmt, '-l', *(str(p) for p in paths)],
                        text=True, capture_output=True, check=True)
if result.stdout:
    print(result.stdout, end='')
    sys.exit('Run make fmt to format the listed Go files.')
print(f'Go formatting OK ({len(paths)} files).')
