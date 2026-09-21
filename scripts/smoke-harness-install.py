#!/usr/bin/env python3
"""Install into a disposable harness profile and probe the actual installed plugin."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('smoke', ROOT / 'scripts/smoke-plugin.py')
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('archive', type=Path)
    p.add_argument('--host', choices=['codex', 'claude'], required=True)
    p.add_argument('--root', type=Path, default=ROOT, help='Source root containing marketplace manifests')
    p.add_argument('--codex', default='codex', help='Codex executable to validate')
    p.add_argument('--node-driver', type=Path, help='Native Windows launch driver')
    args = p.parse_args()
    # Windows CreateProcess does not search PATHEXT for npm's .cmd shim.
    args.codex = shutil.which(args.codex) or args.codex
    driver = (str(args.node_driver), str(Path(__file__).with_name('process-probe.mjs').resolve())) if args.node_driver else ()
    with tempfile.TemporaryDirectory(prefix='tincan isolated harness ') as tmp:
        root = Path(tmp)
        market = root / 'marketplace'
        plugin = smoke.extract(args.archive, market / 'plugins')
        for source in (args.root / 'marketplace').rglob('marketplace.json'):
            target = market / source.relative_to(args.root / 'marketplace')
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target)
        profile = root / 'profile'; profile.mkdir()
        env = dict(os.environ)
        env['CODEX_HOME' if args.host == 'codex' else 'CLAUDE_CONFIG_DIR'] = str(profile)
        if args.host == 'codex':
            commands = [[args.codex, 'plugin', 'marketplace', 'add', str(market), '--json'],
                        [args.codex, 'plugin', 'add', 'tincan@tincan', '--json']]
        else:
            commands = [['claude', 'plugin', 'marketplace', 'add', str(market)],
                        ['claude', 'plugin', 'install', 'tincan@tincan']]
        for command in commands:
            result = subprocess.run(command, env=env, cwd=root, capture_output=True, text=True, encoding='utf-8', timeout=45)
            assert result.returncode == 0, result.stdout + result.stderr
        if args.host == 'codex':
            installed = Path(json.loads(result.stdout)['installedPath'])
        else:
            candidates = list((profile / 'plugins/cache').rglob('.mcp.json'))
            assert len(candidates) == 1, candidates
            installed = candidates[0].parent
        assert (installed / 'release.json').exists(), 'harness installed source rather than release'
        runtime_env = smoke.clean_env(root / 'runtime state')
        manifest = '.codex-plugin/plugin.json' if args.host == 'codex' else '.mcp.json'
        if args.host == 'codex':
            # Use the host's effective command, not command_for(), which repairs
            # relative paths and can hide a real Codex startup failure.
            resolved = subprocess.run([args.codex, 'mcp', 'get', 'tincan', '--json'],
                                      env=env, cwd=root, capture_output=True, text=True, encoding='utf-8', timeout=15)
            assert resolved.returncode == 0, resolved.stderr
            config = json.loads(resolved.stdout)
            transport = config['transport']
            release = json.loads((installed / 'release.json').read_text(encoding='utf-8'))
            if release.get('harness') == 'codex':
                assert config.get('tool_timeout_sec', 0) > 3600, config
                assert transport['args'] == ['plugin', '--host', 'codex'], transport
                assert Path(transport['cwd']) == installed, transport
                assert (Path(transport['cwd']) / transport['command']).parent == installed / 'bin', transport
            runtime_env.update(transport.get('env') or {})
            smoke.probe([transport['command'], *transport.get('args', [])],
                        runtime_env, Path(transport.get('cwd') or root), driver)
        else:
            smoke.probe(smoke.command_for(installed, manifest), runtime_env, root, driver)
        print(f'{args.host}: installed through plugin manager and started installed MCP server successfully; '
              + (f'effective tool timeout: {config.get("tool_timeout_sec")} seconds; ' if args.host == 'codex' else '')
              + 'no model calls or user profile changes.')


if __name__ == '__main__': main()
