import importlib.util
import json
import copy
import shutil
from pathlib import Path
import tempfile
import unittest
import zipfile

spec = importlib.util.spec_from_file_location('packager', Path(__file__).with_name('package-openai-plugin.py'))
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class HostedPackageTest(unittest.TestCase):
    def setUp(self):
        self.root = next(p for p in Path(__file__).resolve().parents if (p / 'plugins/tincan-openai/plugin.json').exists())

    def test_complete_deterministic_remote_package(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive = packager.build(self.root, Path(tmp), 'https://tincan.example', '1.2.3')
            original = archive.read_bytes()
            packager.build(self.root, Path(tmp), 'https://tincan.example', '1.2.3')
            self.assertEqual(original, archive.read_bytes())
            with zipfile.ZipFile(archive) as bundle:
                files = bundle.namelist()
                self.assertFalse(any('/hooks/' in p or '/bin/' in p or p.endswith('.app.json') for p in files))
                manifest = json.loads(bundle.read('tincan/plugin.json'))
                self.assertEqual(manifest['version'], '1.2.3')
                self.assertIn('tincan/skills/tincan-connect/SKILL.md', files)
                self.assertEqual(json.loads(bundle.read('tincan/mcp.json'))['mcpServers']['tincan'],
                    {'type': 'streamable-http', 'url': 'https://tincan.example/mcp?hosted=1'})

    def test_rejects_incompatible_mcp_configuration(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            shutil.copytree(self.root / 'plugins', root / 'plugins')
            shutil.copyfile(self.root / 'LICENSE', root / 'LICENSE')
            path = root / 'plugins/tincan-openai/mcp.json'
            original = json.loads(path.read_text())
            for field, value in [('type', 'http'), ('$schema', 'https://example.com/schema')]:
                with self.subTest(field=field):
                    mcp = copy.deepcopy(original)
                    if field == 'type':
                        mcp['mcpServers']['tincan'][field] = value
                    else:
                        mcp[field] = value
                    path.write_text(json.dumps(mcp))
                    with self.assertRaises(ValueError):
                        packager.build(root, root / 'output')

    def test_submission_manifest_guardrails(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive = packager.build(self.root, Path(tmp))
            with zipfile.ZipFile(archive) as bundle:
                files = {name.removeprefix('tincan/'): bundle.read(name) for name in bundle.namelist()}
            original = json.loads(files['plugin.json'])
            mutations = [
                lambda m: m.update({'$schema': 'https://example.com/schema'}),
                lambda m: m.update({'hooks': {}}),
                lambda m: m['extensions']['com.openai'].update({'hooks': {}}),
                lambda m: m['extensions']['com.openai'].update({'apps': './.app.json'}),
                lambda m: m['extensions']['com.openai']['interface'].pop('logo'),
                lambda m: m['extensions']['com.openai']['interface'].update({'screenshots': ['./missing.png']}),
                lambda m: m['extensions']['com.openai']['review'].update({'demo_recording_url': 'http://example.com/demo'}),
            ]
            for index, mutate in enumerate(mutations):
                with self.subTest(index=index):
                    manifest = copy.deepcopy(original)
                    mutate(manifest)
                    with self.assertRaises(ValueError):
                        packager.validate(manifest, files)

    def test_review_gate_and_unsafe_origins(self):
        with tempfile.TemporaryDirectory() as tmp:
            with self.assertRaises(ValueError):
                packager.build(self.root, Path(tmp), require_review=True)
            for server in ['http://localhost', 'https://token@example.com', 'https://example.com/path', 'https://example.com?token=x', 'https://example.com:invalid', 'https://example.com:70000']:
                with self.assertRaises(ValueError):
                    packager.build(self.root, Path(tmp), server)
            packager.build(self.root, Path(tmp), demo='https://example.com/tincan-demo', require_review=True)


if __name__ == '__main__':
    unittest.main()
