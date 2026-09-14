#!/usr/bin/env python3
"""Build a complete plugin for all supported platforms. Build-time Python only."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit
import zipfile

TARGETS = [f'{system}-{arch}' for system in ('darwin', 'linux', 'windows') for arch in ('amd64', 'arm64')]


def write_json(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + '\n')


def build(root, output, go, server, version, targets, marketplace=None, harness=None):
    output.mkdir(parents=True, exist_ok=True)
    subprocess.run([sys.executable, str(root / 'scripts/build-mls.py')], cwd=root, check=True)
    with tempfile.TemporaryDirectory(prefix='tincan-release-') as tmp:
        plugin = Path(tmp) / 'tincan'
        shutil.copytree(root / 'plugins/tincan', plugin,
                        ignore=shutil.ignore_patterns('bin', '__pycache__', '.DS_Store', '.env', '.env.*'))
        (plugin / 'bin').mkdir(exist_ok=True)
        shutil.copy2(root / '.tools/mls/wasm32-wasip1/release/tincan-mls.wasm', plugin / 'bin/tincan-mls.wasm')
        for manifest_name in ('.claude-plugin/plugin.json', '.codex-plugin/plugin.json', '.cursor-plugin/plugin.json', 'package.json', 'plugin.json'):
            path = plugin / manifest_name
            manifest = json.loads(path.read_text())
            manifest['version'] = version
            if manifest_name == '.codex-plugin/plugin.json' and 'mcpServers' in manifest:
                raise ValueError('Codex must use portable mcp.json; an inline MCP override breaks installed path resolution')
            write_json(path, manifest)
        binaries = {}
        for target in targets:
            system, arch = target.split('-')
            filename = 'tincan.exe' if system == 'windows' else 'tincan'
            binary = plugin / 'bin' / target / filename
            binary.parent.mkdir(parents=True)
            subprocess.run([go, 'build', '-trimpath',
                            f'-ldflags=-s -w -X main.version={version} -X main.defaultServer={server}',
                            '-o', str(binary), './cmd/tincan'], cwd=root,
                           env=dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0'), check=True)
            binary.chmod(0o755)
            binaries[target] = {'path': binary.relative_to(plugin).as_posix(),
                                'sha256': hashlib.sha256(binary.read_bytes()).hexdigest()}
            print(f'Built {target}', flush=True)
        # Keep the stable command identical on every OS. Native Windows process
        # launchers resolve bin/tincan to bin/tincan.exe. Unix executes this script.
        launcher = plugin / 'bin/tincan'
        launcher.write_text('#!/bin/sh\nset -eu\nroot=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)\nexec "$root/scripts/launch.sh" "$@"\n')
        launcher.chmod(0o755)
        (plugin / 'scripts/launch.sh').chmod(0o755)
        if any(target.startswith('windows-') for target in targets):
            subprocess.run([go, 'build', '-trimpath', '-ldflags=-s -w',
                            '-o', str(plugin / 'bin/tincan.exe'), './cmd/tincan-launcher'], cwd=root,
                           env=dict(os.environ, GOOS='windows', GOARCH='386', CGO_ENABLED='0'), check=True)
        if harness in ('cursor', 'codex'):
            (plugin / 'plugin.json').unlink()  # Select the harness's native manifest.
        if harness == 'codex':
            # Codex 0.153.4's portable MCP schema rejects timeout fields and
            # plugin user-policy overrides do not apply them. Native MCP config
            # supports the timeout. Its explicit cwd resolves to the installed
            # root; legacy config does not expand CLAUDE_PLUGIN_ROOT in commands.
            manifest_path = plugin / '.codex-plugin/plugin.json'
            manifest = json.loads(manifest_path.read_text())
            manifest['mcpServers'] = './.codex-plugin/mcp.json'
            write_json(manifest_path, manifest)
            write_json(plugin / '.codex-plugin/mcp.json', {'mcpServers': {'tincan': {
                'command': './bin/tincan',
                'args': ['plugin', '--host', 'codex'],
                'cwd': '.',
                'tool_timeout_sec': 3660,
            }}})
        yaml_manifest = plugin / 'plugin.yaml'
        yaml_manifest.write_text(re.sub(r'^version: .*$', 'version: ' + version, yaml_manifest.read_text(), flags=re.M))
        shutil.copy2(root / 'LICENSE', plugin / 'LICENSE')
        for name in ('E2EE.md', 'CLOUD_AGENTS.md', 'AGENT_METADATA.md', 'AGENT_TEXT.md', 'ONBOARDING_RELEASE_GATE.md', 'ONBOARDING.md', 'CLIENTS.md', 'HARNESS_DELIVERY.md', 'CLAUDE_WAKE.md'):
            (plugin / 'docs').mkdir(exist_ok=True)
            shutil.copy2(root / 'docs' / name, plugin / 'docs' / name)
            doc = plugin / 'docs' / name
            doc.write_text(doc.read_text().replace('../plugins/tincan/skills/', '../skills/'))
        shutil.copytree(root / 'sdk', plugin / 'sdk', ignore=shutil.ignore_patterns('__pycache__', '*.pyc'))
        (plugin / 'README.md').write_text(
            '# Tincan\n\nSee [client setup](docs/CLIENTS.md) for Cursor, Copilot CLI, OpenClaw, and Hermes.\n\nInstall the plugin in your harness, then ask “Connect me to Tincan” '
            'or paste a Tincan invite link. The plugin selects its bundled executable automatically. '
            'No Go, Python, Node, package manager, or separate CLI installation is required.\n\n'
            'Credentials and connection state are stored outside the plugin installation directory. '
            'Update through your plugin manager. Background wakeups depend on host capabilities '
            'and may require the host’s normal trust or channel approval.\n')
        # Hash every shipped executable/launcher, including the Windows dispatcher.
        files = {p.relative_to(plugin).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
                 for p in sorted(plugin.rglob('*')) if p.is_file()}
        write_json(plugin / 'release.json', {'version': version, 'server': server, 'harness': harness or 'portable',
                   'targets': binaries, 'files': files, 'protocol': 'tincan/1', 'cgo': False,
                   'onboarding_digest': json.loads((plugin / 'onboarding-contract.json').read_text())['digest']})
        filename = 'tincan-plugin.zip' if targets == TARGETS else f'tincan-{targets[0]}.zip'
        if harness:
            filename = filename.replace('tincan-', 'tincan-' + harness + '-', 1)
        artifact = output / filename
        with zipfile.ZipFile(artifact, 'w', zipfile.ZIP_DEFLATED) as archive:
            for file in sorted(plugin.rglob('*')):
                if file.is_file(): archive.write(file, file.relative_to(plugin.parent))
        (output / 'SHA256SUMS').write_text(''.join(f'{hashlib.sha256(item.read_bytes()).hexdigest()}  {item.name}\n' for item in sorted(output.glob('*.zip'))))
        archives = {}
        for item in sorted(output.glob('*.zip')):
            with zipfile.ZipFile(item) as bundle:
                release = json.loads(bundle.read('tincan/release.json'))
            archives[item.name] = {'sha256': hashlib.sha256(item.read_bytes()).hexdigest(),
                'size': item.stat().st_size, 'version': release['version'],
                'server': release['server'], 'targets': sorted(release['targets']),
                'onboarding_digest': release['onboarding_digest']}
        write_json(output / 'manifest.json', {'schema_version': 1, 'archives': archives})
        if marketplace:
            if marketplace.exists(): raise ValueError('marketplace output must be a fresh directory')
            shutil.copytree(plugin, marketplace / 'plugins/tincan')
            for name in ('.agents/plugins/marketplace.json', '.claude-plugin/marketplace.json'):
                source = root / 'marketplace' / name
                target = marketplace / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source, target)
            (marketplace / 'README.md').write_text(
                '# Tincan plugin marketplace\n\n'
                f'Built from Tincan {version}. Contains ready-to-run binaries for every supported platform.\n\n'
                'Install Tincan through your harness, then prompt “Connect me to Tincan”.\n\n'
                'Source and installation details: https://github.com/tincan-ai/tincan-plugin\n')
        print(f'Packaged {artifact}', flush=True)
        return artifact


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--output', type=Path)
    parser.add_argument('--go', default='go')
    parser.add_argument('--server', default=os.environ.get('TINCAN_RELEASE_SERVER', 'https://app.gotincan.com'))
    parser.add_argument('--version')
    parser.add_argument('--target', choices=TARGETS, help='Optional single-platform developer package')
    parser.add_argument('--harness', choices=['cursor', 'codex'], help='Native harness package (Cursor hooks or Codex listener timeout)')
    parser.add_argument('--marketplace', type=Path, help='Write the complete marketplace tree for Git distribution')
    args = parser.parse_args()
    if not args.server:
        parser.error('--server or TINCAN_RELEASE_SERVER must specify the public HTTPS service origin')
    url = urlsplit(args.server)
    if url.scheme != 'https' or not url.hostname or url.username or url.password or url.query or url.fragment or url.path not in ('', '/'):
        parser.error('--server must be an HTTPS origin with no credentials, path, query, or fragment')
    root = args.root.resolve()
    version = args.version or json.loads((root / 'plugins/tincan/.claude-plugin/plugin.json').read_text())['version']
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', version):
        parser.error('version must be a semantic version')
    build(root, (args.output or root / 'dist/releases').resolve(), args.go,
          args.server.rstrip('/'), version, [args.target] if args.target else TARGETS,
          args.marketplace.resolve() if args.marketplace else None, args.harness)


if __name__ == '__main__':
    main()
