#!/usr/bin/env python3
"""Check relative Markdown file links offline; leave remote URLs and anchors alone."""
from pathlib import Path
import re
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
errors = []
count = 0
for path in sorted(ROOT.rglob('*.md')):
    if any(part in {'.git', 'build', 'dist', 'third_party'}
           for part in path.relative_to(ROOT).parts):
        continue
    body = re.sub(r'^```.*?^```[^\n]*', '', path.read_text(), flags=re.M | re.S)
    for match in re.finditer(r'!?\[[^\]\n]*\]\(([^\n]*?)\)', body):
        target = match[1].strip()
        if target.startswith('<'):
            target = target[1:target.index('>')]
        else:
            target = target.split()[0] if target else ''
        parsed = urlsplit(target)
        if not parsed.path or parsed.scheme or parsed.netloc:
            continue
        count += 1
        if parsed.path.startswith('/'):
            errors.append(f'{path.relative_to(ROOT)}: use a relative link: {target}')
        elif not (path.parent / unquote(parsed.path)).exists():
            errors.append(f'{path.relative_to(ROOT)}: missing {target}')
if errors:
    print('\n'.join(errors), file=sys.stderr)
    sys.exit(1)
print(f'Relative Markdown links OK ({count} file links).')
