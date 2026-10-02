#!/usr/bin/env python3
"""Fetch the pinned, official Claude mod declarations for contributor typechecks."""
import argparse
import hashlib
from pathlib import Path
import urllib.request

REVISION = '52c76441cae91f6891e4712306bffb057ff6fec5'
DIGEST = '8ae1244d19d4b393261605fe72d517b66be0d7e1c214c378cdec5107e8b0b46c'
URL = f'https://raw.githubusercontent.com/anthropics/claude-code/{REVISION}/mods/types/claude-code.d.ts'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=Path('.tools/claude-mod-upstream/types/claude-code.d.ts'))
    args = parser.parse_args()
    if args.output.is_file() and hashlib.sha256(args.output.read_bytes()).hexdigest() == DIGEST:
        print('Pinned Claude mod declarations are already available.')
        return
    with urllib.request.urlopen(URL, timeout=30) as response:
        data = response.read(2 << 20)
    if hashlib.sha256(data).hexdigest() != DIGEST:
        raise ValueError('Official Claude mod declarations did not match the pinned checksum')
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_bytes(data)
    print(f'Fetched official mod declarations at {REVISION} to {args.output}')


if __name__ == '__main__':
    main()
