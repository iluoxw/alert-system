#!/usr/bin/env python3
"""L4 subprocess environment contract, with synthetic values and no network/CLI execution."""
import contextlib
import io
import json
import os
from pathlib import Path
import runpy
import subprocess
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name('l4_validate.py')
PROXY_NAMES = ('HTTPS_PROXY', 'HTTP_PROXY', 'ALL_PROXY', 'NO_PROXY',
               'https_proxy', 'http_proxy', 'all_proxy', 'no_proxy')


class L4EnvironmentTests(unittest.TestCase):
    def execute(self, environment, exit_code=0):
        # A real pipe exercises the existing bounded reader without starting any child process.
        read_fd, write_fd = os.pipe()
        os.write(write_fd, b'{"data":[]}')
        os.close(write_fd)
        child_output = os.fdopen(read_fd, 'rb')
        self.addCleanup(child_output.close)
        captured = {}

        class FakeProcess:
            stdout = child_output
            def poll(self): return exit_code
            def wait(self, timeout): return exit_code

        def launch(argv, **kwargs):
            captured.update(argv=argv, **kwargs)
            return FakeProcess()

        output = io.StringIO()
        with patch.dict(os.environ, environment, clear=True), \
                patch.object(subprocess, 'Popen', side_effect=launch), \
                contextlib.redirect_stdout(output):
            runpy.run_path(str(SCRIPT), run_name='__main__')
        return captured, json.loads(output.getvalue()), output.getvalue()

    def test_preserves_both_proxy_casings_and_certificates_without_unrelated_environment(self):
        environment = {name: 'synthetic-proxy-' + name for name in PROXY_NAMES}
        environment.update(PATH='/synthetic/path', HOME='/synthetic/home',
                           CCLOUD_API_KEY='synthetic-private-token',
                           CCLOUD_API_BASE_URL='synthetic-base',
                           SSL_CERT_FILE='/synthetic/ca.pem', NODE_EXTRA_CA_CERTS='/synthetic/extra-ca.pem',
                           NODE_OPTIONS='synthetic-preload-must-not-propagate',
                           OMA_API_KEY='synthetic-oma-token', OBS_AUTH='synthetic-observation-auth',
                           CCLOUD_API_MODE='mock', CCLOUD_DISABLE_UPDATE_CHECK='0',
                           UNRELATED_VARIABLE='synthetic-unrelated')
        captured, result, output = self.execute(environment)
        child = captured['env']
        for name in PROXY_NAMES + ('SSL_CERT_FILE', 'NODE_EXTRA_CA_CERTS', 'CCLOUD_API_KEY'):
            self.assertEqual(child.get(name), environment[name], name + ' not preserved')
        expected = set(PROXY_NAMES) | {'PATH', 'HOME', 'CCLOUD_API_KEY', 'CCLOUD_API_BASE_URL',
                                      'SSL_CERT_FILE', 'NODE_EXTRA_CA_CERTS',
                                      'CCLOUD_API_MODE', 'CCLOUD_DISABLE_UPDATE_CHECK'}
        self.assertEqual(set(child), expected)
        self.assertEqual(child['CCLOUD_API_MODE'], 'real')
        self.assertEqual(child['CCLOUD_DISABLE_UPDATE_CHECK'], '1')
        self.assertEqual(captured['stderr'], subprocess.DEVNULL)
        self.assertEqual(result['status'], 'observed')
        self.assertIsNone(result['desired_replicas'])
        self.assertNotIn('synthetic-', output)
        self.assertEqual(captured['argv'][1:4], ['app', 'pods', '--app-id'])
        self.assertEqual(captured['argv'][-2:], ['--cluster-id', '60'])
        self.assertEqual(len(captured['argv']), 7)

    def test_missing_proxy_values_are_not_fabricated_and_query_failure_stays_unknown(self):
        captured, result, output = self.execute({'NODE_OPTIONS': 'synthetic-forbidden'}, exit_code=2)
        self.assertFalse(set(PROXY_NAMES) & set(captured['env']))
        self.assertNotIn('NODE_OPTIONS', captured['env'])
        self.assertEqual(result['status'], 'unknown')
        self.assertEqual(result['reason'], 'query_failed')
        self.assertEqual(result['cli_exit_code'], 2)
        self.assertIsNone(result['desired_replicas'])
        self.assertNotIn('synthetic-', output)


if __name__ == '__main__':
    unittest.main()
