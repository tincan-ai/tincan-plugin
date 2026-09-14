#!/usr/bin/env python3
"""Verify the shipped package, then speak MCP through its manifest launch command."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import tempfile
import zipfile


def extract(archive, dest):
    with zipfile.ZipFile(archive) as bundle:
        bundle.extractall(dest)
        for entry in bundle.infolist():
            if not entry.is_dir():
                mode = (entry.external_attr >> 16) & 0o777
                if mode: (dest / entry.filename).chmod(mode)
    return dest / 'tincan'


def command_for(plugin, manifest):
    path = plugin / manifest
    manifest_data = json.loads(path.read_text())
    if manifest == '.codex-plugin/plugin.json':
        servers = manifest_data.get('mcpServers', './mcp.json')
        if isinstance(servers, str):
            manifest_data = json.loads((plugin / servers).read_text())
    config = manifest_data['mcpServers']['tincan']
    command = config['command'].replace('${CLAUDE_PLUGIN_ROOT}', str(plugin))
    if command.startswith('./'): command = str(plugin / command[2:])
    return [command, *config['args']]


def clean_env(state):
    # The launched plugin gets OS utilities only: no Go, Node, Python, curl,
    # developer credentials, or separately installed tincan on PATH.
    keep = ('SystemRoot', 'SYSTEMROOT', 'WINDIR', 'COMSPEC', 'TEMP', 'TMP', 'PATHEXT')
    env = {k: os.environ[k] for k in keep if k in os.environ}
    env.update(HOME=str(state), USERPROFILE=str(state), XDG_CONFIG_HOME=str(state),
               APPDATA=str(state), TINCAN_STATE_DIR=str(state / 'connections'))
    env['PATH'] = str(Path(os.environ.get('SystemRoot', 'C:/Windows')) / 'System32') if os.name == 'nt' else '/usr/bin:/bin:/usr/sbin'
    return env


def probe(command, env, cwd, driver=()):
    messages = [
        {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {
            'protocolVersion': '2025-03-26', 'capabilities': {},
            'clientInfo': {'name': 'tincan-package-smoke', 'version': '1.0.0'}}},
        {'jsonrpc': '2.0', 'method': 'notifications/initialized'},
        {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/list', 'params': {}},
    ]
    # Keep stdin open until the response arrives: EOF can cancel in-flight MCP requests.
    child = subprocess.Popen([*driver, *command], cwd=cwd, env=env, stdin=subprocess.PIPE,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    import threading
    watchdog = threading.Timer(20, child.kill)
    watchdog.start()
    try:
        child.stdin.write(json.dumps(messages[0]) + '\n'); child.stdin.flush()
        initialized = json.loads(child.stdout.readline())
        assert initialized.get('id') == 1 and 'result' in initialized, initialized
        for message in messages[1:]: child.stdin.write(json.dumps(message) + '\n')
        child.stdin.flush()
        while True:
            line = child.stdout.readline()
            assert line, child.stderr.read()
            result = json.loads(line)
            if result.get('id') == 2: break
        tools = {t['name'] for t in result['result']['tools']}
        assert {'tincan_connect', 'tincan_status', 'message_send', 'inbox_claim', 'inbox_wait'} <= tools, tools
        child.stdin.close()
        assert child.wait(timeout=10) == 0, child.stderr.read()
    finally:
        watchdog.cancel()
        if child.poll() is None: child.kill(); child.wait()
    return initialized


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive', type=Path)
    parser.add_argument('--universal', action='store_true')
    parser.add_argument('--node-driver', type=Path, help='Windows test driver using native Node/libuv process resolution')
    args = parser.parse_args()
    driver = [str(args.node_driver.resolve()), str(Path(__file__).with_name('process-probe.mjs').resolve())] if args.node_driver else []
    with tempfile.TemporaryDirectory(prefix='tincan package with spaces ') as temporary:
        root = Path(temporary)
        plugin = extract(args.archive, root)
        metadata = json.loads((plugin / 'release.json').read_text())
        if args.universal: assert len(metadata['targets']) == 6
        for name, digest in metadata['files'].items():
            assert hashlib.sha256((plugin / name).read_bytes()).hexdigest() == digest, name
        # Test the unmodified extensionless manifest path. Python CreateProcess
        # uses different lookup rules than Rust/Node on Windows; use a host-like
        # native process driver there instead of rewriting the manifest command.
        state = root / 'private state'; state.mkdir()
        env = clean_env(state)
        assert (plugin / 'bin/tincan-mls.wasm').read_bytes()[:4] == b'\x00asm'
        crypto = subprocess.run([*driver, *command_for(plugin, '.mcp.json')[:1], 'encryption-check'],
                                text=True, capture_output=True, env=env, timeout=20)
        assert crypto.returncode == 0 and json.loads(crypto.stdout)['ok'], crypto.stderr
        for name in ('.codex-plugin/plugin.json', '.mcp.json', 'mcp.json'):
            initialized = probe(command_for(plugin, name), env, root, driver)
            assert initialized['result']['serverInfo']['version'] == metadata['version']
        # Launch each documented client entry, including paths containing spaces.
        for host in ('cursor', 'copilot', 'openclaw', 'hermes'):
            config = json.loads((plugin / 'clients' / (host + '.json')).read_text())
            servers = config['mcp']['servers'] if host == 'openclaw' else config['mcp_servers' if host == 'hermes' else 'mcpServers']
            entry = servers['tincan']
            assert entry['args'] == ['plugin', '--host', host]
            command = entry['command'].replace('/absolute/path/to/tincan', str(plugin))
            result = probe([command, *entry['args']], env, root, driver)
            assert 'claude/channel' not in result['result'].get('capabilities', {}).get('experimental', {})
        versions = {json.loads((plugin / name).read_text())['version']
                    for name in ('plugin.json', '.claude-plugin/plugin.json', '.codex-plugin/plugin.json', '.cursor-plugin/plugin.json', 'package.json') if (plugin / name).exists()}
        assert versions == {metadata['version']}, versions
        for name in ('openclaw.plugin.json', 'plugin.yaml', 'native/openclaw/index.mjs',
                     'native/hermes/adapter.py', 'sdk/python/run_cursor.py', 'sdk/python/run_copilot.py',
                     'hooks/claude.json', 'hooks/cursor.json', 'com.github.copilot/hooks/hooks.json'):
            assert (plugin / name).is_file(), name
        assert json.loads((plugin / '.claude-plugin/plugin.json').read_text())['hooks'] == './hooks/claude.json'
        for host, event in (('copilot', 'SessionStart'), ('cursor', 'SessionStart'), ('claude', 'SessionStart')):
            result = subprocess.run([*driver, *command_for(plugin, '.mcp.json')[:1], 'harness-hook', host, event],
                                    input=json.dumps({'session_id': 'smoke-session'}), text=True, capture_output=True, env=env, timeout=10)
            assert result.returncode == 0 and 'hook_session_id' in result.stdout, result.stderr

        assert not (state / 'connections').exists(), 'discovery created credentials before the first prompt'
        # Exercise argument forwarding and local sidecar discovery separately.
        command = [*driver, *command_for(plugin, '.mcp.json')[:1]]
        result = subprocess.run(command + ['sidecar', '--host', 'smoke', '--state-dir', str(state / 'sidecar')],
                                env=env, cwd=root, input='{"id":1,"method":"tools"}\n',
                                capture_output=True, text=True, timeout=10, check=True)
        assert json.loads(result.stdout.splitlines()[0])['protocol'] == 'tincan/1'
        assert not (state / 'sidecar').exists()
        result = subprocess.run(command + ['not-a-command'], env=env, cwd=root, capture_output=True, timeout=10)
        assert result.returncode != 0, 'launcher swallowed child failure'
        for binary in metadata['targets'].values():
            path = plugin / binary['path']; path.rename(str(path) + '.missing')
        failure = subprocess.run(command, env=env, cwd=root, capture_output=True, text=True, timeout=10)
        assert failure.returncode == 126 and 'Reinstall' in failure.stderr, failure.stderr
        print(f'Universal plugin verified on {platform.system()} {platform.machine()}: MCP, sidecar, version, checksums, exit status, and no setup-time credentials.')


if __name__ == '__main__': main()
