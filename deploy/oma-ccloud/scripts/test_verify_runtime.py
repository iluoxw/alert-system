#!/usr/bin/env python3
"""Offline integrity checks against fixtures and the operator-selected real npm package."""
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import install_runtime as runtime

ROOT = Path(__file__).resolve().parents[1]


def bundle(path, version='2.2.1', duplicate=False):
    package = json.dumps({'name': '@cvte/ccloud-cli', 'version': version}).encode()
    files = {'package.json': package, 'dist/ccloud-cli.js': b'throw new Error("official CLI must not run");'}
    manifest = {'package': '@cvte/ccloud-cli', 'version': '2.2.1',
                'files': [{'path': name, 'sha256': hashlib.sha256(raw).hexdigest()}
                          for name, raw in files.items()]}
    files['BUNDLE.json'] = json.dumps(manifest).encode()
    with tarfile.open(path, 'w:gz') as tar:
        for name, raw in files.items():
            item = tarfile.TarInfo('ccloud-cli/' + name); item.size = len(raw); item.mode = 0o644
            tar.addfile(item, io.BytesIO(raw))
        if duplicate:
            tar.addfile(item, io.BytesIO(raw))
    return hashlib.sha256(path.read_bytes()).hexdigest()


class VerificationTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.archive = self.root / 'bundle.tgz'
        self.sha = bundle(self.archive)
        self.dest = self.root / 'runtime'
        self.guard = ROOT / 'ccloud-cli'
        # This fixture tests package checks without depending on the test host's Node version.
        self.node = patch.object(runtime, 'node_version', return_value='v24.0.0')
        self.node.start(); self.addCleanup(self.node.stop)
        runtime.install_runtime(self.archive, self.sha, self.guard, self.dest)

    def verify(self):
        return runtime.verify_runtime(self.archive, self.sha, self.guard, self.dest)

    def test_installed_package_and_guard_pass_without_official_entrypoint(self):
        result = self.verify()
        self.assertTrue(result['installed']); self.assertTrue(result['file_hashes_verified'])
        self.assertTrue(result['guard_verified']); self.assertFalse(result['official_cli_executed'])
        self.assertEqual(result['package'], '@cvte/ccloud-cli'); self.assertEqual(result['version'], '2.2.1')

    def test_tampering_missing_extra_symlink_and_guard_fail_closed(self):
        for kind in ('tamper', 'missing', 'extra', 'symlink', 'guard', 'guard-executable'):
            with self.subTest(kind=kind):
                runtime.install_runtime(self.archive, self.sha, self.guard, self.dest)
                target = self.dest / 'ccloud-cli/dist/ccloud-cli.js'
                if kind == 'tamper': target.write_text('changed')
                if kind == 'missing': target.unlink()
                if kind == 'extra': (self.dest / 'ccloud-cli/extra').write_text('unexpected')
                if kind == 'symlink': target.unlink(); target.symlink_to(self.guard)
                if kind == 'guard':
                    path = self.dest / 'bin/ccloud-cli'; path.chmod(0o700); path.write_text('#!/bin/sh\nexit 0\n')
                if kind == 'guard-executable': (self.dest / 'bin/ccloud-cli').chmod(0o400)
                with self.assertRaises(ValueError): self.verify()
                # Remove the fixture tree rather than asking the installer to adopt unsafe files.
                import shutil
                shutil.rmtree(self.dest)

    def test_archive_digest_version_and_duplicate_members_rejected(self):
        with self.assertRaises(ValueError): runtime.read_bundle(self.archive, '0' * 64)
        bad = self.root / 'bad.tgz'
        sha = bundle(bad, version='9.9.9')
        with self.assertRaisesRegex(ValueError, 'identity/version'): runtime.read_bundle(bad, sha)
        sha = bundle(bad, duplicate=True)
        with self.assertRaisesRegex(ValueError, 'duplicate'): runtime.read_bundle(bad, sha)

    def test_only_node_version_and_guard_denial_may_execute_with_no_inherited_env(self):
        self.node.stop()
        real_run = subprocess.run
        guard = str(self.dest.parent.resolve() / self.dest.name / 'bin/ccloud-cli')
        def checked(args, **kw):
            self.assertEqual(kw['env'], {'PATH': os.defpath})
            self.assertTrue(args == [guard] or (len(args) == 2 and args[1] == '--version'
                                              and Path(args[0]).name == 'node'))
            self.assertNotIn('ccloud-cli.js', ' '.join(args))
            return real_run(args, **kw)
        # NODE_OPTIONS would make node load arbitrary code if the parent environment leaked.
        with patch.dict(os.environ, {'NODE_OPTIONS': '--require=/DO_NOT_LOAD',
                                    'CCLOUD_API_KEY': 'DO_NOT_FORWARD'}), \
                patch.object(runtime.subprocess, 'run', side_effect=checked), \
                patch('socket.create_connection', side_effect=AssertionError('network forbidden')):
            self.assertTrue(self.verify()['installed'])

    def test_missing_or_old_node_fails_without_cli_execution(self):
        self.node.stop()
        with patch.object(runtime.shutil, 'which', return_value=None), \
                patch.object(runtime.subprocess, 'run', return_value=subprocess.CompletedProcess([], 64, b'', b'')) as run:
            with self.assertRaisesRegex(ValueError, 'Node'): self.verify()
            # The only executed subprocess is the trusted guard's empty-argv denial.
            self.assertEqual(run.call_count, 1)
        with patch.object(runtime.shutil, 'which', return_value='/trusted/node'), \
                patch.object(runtime.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, 'v18.14.0\n', '')):
            with self.assertRaisesRegex(ValueError, 'Node'): runtime.node_version()


class RealPackageTests(unittest.TestCase):
    @unittest.skipUnless(os.environ.get('CCLOUD_TEST_PACKAGE_DIR'), 'operator must select public npm package')
    def test_real_offline_bundle_install_and_verify(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive = Path(tmp) / 'runtime.tgz'
            packed = subprocess.run([sys.executable, str(ROOT / 'scripts/pack_runtime.py'),
                                     '--package-dir', os.environ['CCLOUD_TEST_PACKAGE_DIR'],
                                     '--output', str(archive)], check=True, capture_output=True, text=True)
            sha = json.loads(packed.stdout)['archive_sha256']
            dest = Path(tmp) / 'runtime'
            result = runtime.install_runtime(archive, sha, ROOT / 'ccloud-cli', dest)
            self.assertTrue(result['installed'])
            self.assertTrue(runtime.verify_runtime(archive, sha, ROOT / 'ccloud-cli', dest)['file_hashes_verified'])


if __name__ == '__main__':
    unittest.main()
