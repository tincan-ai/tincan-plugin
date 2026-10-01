#!/usr/bin/env python3
"""Build Tincan's hosted OpenAI directory package without local hooks or binaries."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import tempfile
from urllib.parse import urlsplit
import zipfile


def https_url(value, origin=False):
    parsed = urlsplit(value)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password or parsed.fragment:
        raise ValueError('Expected an HTTPS URL without credentials or a fragment')
    # Reading port also validates malformed or out-of-range authority values.
    _ = parsed.port
    if origin and (parsed.query or parsed.path not in ('', '/')):
        raise ValueError('The server must be an HTTPS origin')
    return value.rstrip('/') if origin else value


def validate(manifest, files, require_review=False):
    if not re.fullmatch(r'[a-z0-9]+(?:-[a-z0-9]+)*', manifest['name']) or len(manifest['name']) > 64:
        raise ValueError('Expected a stable lowercase plugin name with single hyphens')
    ui = manifest['extensions']['com.openai']['interface']
    for field, limit in [('displayName', 30), ('shortDescription', 30), ('longDescription', 4000), ('developerName', 80)]:
        if not isinstance(ui.get(field), str) or not 0 < len(ui[field]) <= limit:
            raise ValueError(f'{field} must contain 1–{limit} characters')
    for field in ('websiteURL', 'supportURL', 'privacyPolicyURL', 'termsOfServiceURL'):
        https_url(ui[field])
        if len(ui[field]) > 1024:
            raise ValueError(f'{field} must be at most 1024 characters')
    if not ui.get('category'):
        raise ValueError('Choose a listing category')
    prompts = ui.get('defaultPrompt', [])
    prompts = [prompts] if isinstance(prompts, str) else prompts
    if len(prompts) > 3 or any(not isinstance(p, str) or not 0 < len(p) <= 128 for p in prompts) or len(set(prompts)) != len(prompts):
        raise ValueError('Use up to three distinct starter prompts of at most 128 characters')
    for field in ('logo', 'composerIcon', 'logoDark'):
        if field in ui and ui[field].removeprefix('./') not in files:
            raise ValueError(f'Missing asset for {field}')
    extension = manifest['extensions']['com.openai']
    if any(key in extension.get('review', {}) for key in ('test_credentials', 'reviewer_instructions')):
        raise ValueError('Reviewer access belongs in the secure dashboard, never the package')
    path = extension['onboardingSkill']
    if not path.startswith('./skills/') or '..' in Path(path).parts or path.removeprefix('./') not in files:
        raise ValueError('onboardingSkill must refer to an included skill')
    cases = extension['review']['test_cases']
    if len(cases['positive']) != 5 or len(cases['negative']) != 3:
        raise ValueError('MCP review requires exactly five positive and three negative cases')
    for case in cases['positive']:
        if not all(case.get(k) for k in ('description', 'prompt', 'tools_triggered', 'expected_behavior')):
            raise ValueError('Incomplete positive review case')
    for case in cases['negative']:
        if not all(case.get(k) for k in ('description', 'prompt')):
            raise ValueError('Incomplete negative review case')
    if require_review and not extension['review'].get('demo_recording_url'):
        raise ValueError('Supply --demo-recording-url before claiming the package is review-ready')
    for path in files:
        if Path(path).parts[0] not in ('skills', 'assets', 'plugin.json', 'mcp.json', 'LICENSE', 'README.md'):
            raise ValueError(f'Unsupported hosted package file: {path}')


def build(root, output, server='https://app.gotincan.com', version=None, demo=None, require_review=False):
    server = https_url(server, origin=True)
    source = root / 'plugins/tincan-openai'
    manifest = json.loads((source / 'plugin.json').read_text())
    canonical = json.loads((root / 'plugins/tincan/plugin.json').read_text())
    manifest['version'] = version or canonical['version']
    if not re.fullmatch(r'\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?', manifest['version']):
        raise ValueError('Expected a semantic version')
    if demo:
        manifest['extensions']['com.openai']['review']['demo_recording_url'] = https_url(demo)
    mcp = json.loads((source / 'mcp.json').read_text())
    if set(mcp) - {'$schema', 'mcpServers'} or set(mcp['mcpServers']) != {'tincan'} or set(mcp['mcpServers']['tincan']) != {'type', 'url'} or mcp['mcpServers']['tincan']['type'] != 'http':
        raise ValueError('The hosted package requires one remote HTTP server without inline credentials')
    mcp['mcpServers']['tincan']['url'] = server + '/mcp?hosted=1'
    files = {}
    for path in sorted((source / 'skills').rglob('*')):
        if path.is_file() and path.suffix == '.md' and not path.is_symlink():
            files[path.relative_to(source).as_posix()] = path.read_bytes()
    for path in sorted((root / 'plugins/tincan/assets').glob('*')):
        if path.is_file() and path.suffix in ('.png', '.svg') and not path.is_symlink():
            files['assets/' + path.name] = path.read_bytes()
    files['plugin.json'] = (json.dumps(manifest, indent=2) + '\n').encode()
    files['mcp.json'] = (json.dumps(mcp, indent=2) + '\n').encode()
    files['LICENSE'] = (root / 'LICENSE').read_bytes()
    files['README.md'] = b'# Tincan for ChatGPT and Codex\n\nAuthorize your Tincan connection through the host, then ask to open your rooms. Each authorization creates a distinct agent; reopening the panel preserves it. Independent collaborators need independent authorizations. Hosted connections support standard rooms. Event watches require host subscription support.\n'
    validate(manifest, files, require_review)
    output.mkdir(parents=True, exist_ok=True)
    target = output / 'tincan-openai-plugin.zip'
    with tempfile.TemporaryDirectory(prefix='tincan-openai-') as tmp:
        staged = Path(tmp) / target.name
        with zipfile.ZipFile(staged, 'w', zipfile.ZIP_DEFLATED) as archive:
            for name, data in sorted(files.items()):
                # Fixed timestamps make repeated builds of the same inputs identical.
                info = zipfile.ZipInfo('tincan/' + name, date_time=(2026, 1, 1, 0, 0, 0))
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = 0o100644 << 16
                archive.writestr(info, data)
        target.write_bytes(staged.read_bytes())
    digest = hashlib.sha256(target.read_bytes()).hexdigest()
    (output / 'SHA256SUMS').write_text(f'{digest}  {target.name}\n')
    (output / 'manifest.json').write_text(json.dumps({'version': manifest['version'], 'server': server,
        'archive': target.name, 'sha256': digest, 'review_ready': bool(manifest['extensions']['com.openai']['review'].get('demo_recording_url')),
        'files': {name: hashlib.sha256(data).hexdigest() for name, data in sorted(files.items())}}, indent=2) + '\n')
    print(f'Packaged {target}')
    if not manifest['extensions']['com.openai']['review'].get('demo_recording_url'):
        print('Review material remaining: a reviewer-accessible demo recording URL and secure dashboard access setup.')
    return target


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--output', type=Path)
    parser.add_argument('--server', default='https://app.gotincan.com')
    parser.add_argument('--version')
    parser.add_argument('--demo-recording-url')
    parser.add_argument('--require-review-ready', action='store_true')
    args = parser.parse_args()
    root = args.root.resolve()
    try:
        build(root, (args.output or root / 'dist/openai').resolve(), args.server, args.version,
              args.demo_recording_url, args.require_review_ready)
    except (ValueError, KeyError) as error:
        parser.error(str(error))


if __name__ == '__main__':
    main()
