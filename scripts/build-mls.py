#!/usr/bin/env python3
"""Build the pinned OpenMLS WASI module once for all Tincan platforms."""
import os
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parents[1]
target = ROOT / '.tools/mls'
env = dict(os.environ)
# Optional workspace-local compiler, useful with Homebrew Rust (whose standard
# library metadata is incompatible with the official WASI target).
local = ROOT / '.tools/rust-wasi/bin/rustc'
if local.exists() and 'RUSTC' not in env:
    env['RUSTC'] = str(local)
subprocess.run(['cargo', 'build', '--locked', '--release', '--target', 'wasm32-wasip1',
                '--manifest-path', str(ROOT / 'crypto/mls/Cargo.toml'),
                '--target-dir', str(target)], cwd=ROOT, env=env, check=True)
module = target / 'wasm32-wasip1/release/tincan-mls.wasm'
print(module)
