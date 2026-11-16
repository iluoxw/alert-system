import copy
import contextlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import canary_rule as c


class FakeGrafana:
    def __init__(self):
        self.rules = [{'uid': 'real-production', 'folderUID': c.FOLDER, 'ruleGroup': 'IAC Go production', 'threshold': 99}]
        self.original = copy.deepcopy(self.rules[0])
        self.calls = []

    def __call__(self, method, path, body=None):
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == 'GET':
            return copy.deepcopy(self.rules)
        if method == 'POST':
            assert not any(r['uid'] == body['uid'] for r in self.rules)
            self.rules.append(copy.deepcopy(body))
        elif method == 'PUT':
            self.rules = [copy.deepcopy(body) if r['uid'] == c.UID else r for r in self.rules]
        elif method == 'DELETE':
            self.rules = [r for r in self.rules if r['uid'] != c.UID]
        return None

    def mutations(self):
        return [x for x in self.calls if x[0] != 'GET']


class CanaryTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.state = Path(self.temp.name) / 'state.json'
        self.api = FakeGrafana()
        self.patcher = patch.object(c, 'request', self.api)
        self.patcher.start(); self.addCleanup(self.patcher.stop)

    def run_action(self, action='fire', apply=True):
        with contextlib.redirect_stdout(io.StringIO()):
            return c.run(action, apply, self.state)

    def test_default_plan_only_reads_and_creates_no_state_or_lock(self):
        self.run_action(apply=False)
        self.assertEqual(self.api.mutations(), [])
        self.assertEqual(list(Path(self.temp.name).iterdir()), [])

    def test_create_resolve_cleanup_and_repeat_preserve_production(self):
        self.run_action()
        self.assertEqual(stat.S_IMODE(self.state.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(Path(str(self.state)+'.lock').stat().st_mode), 0o600)
        self.assertEqual(self.api.rules[-1]['data'][0]['model']['expr'], 'vector(1)')
        self.assertEqual(self.api.rules[-1]['labels'], {'product':'IAC','sre.rule.id':c.UID,'sre.canary':'true'})
        self.run_action()  # 已验证的重复fire不重写。
        self.assertEqual([x[0] for x in self.api.mutations()], ['POST'])
        self.run_action('resolve')
        self.assertEqual(self.api.rules[-1]['data'][0]['model']['expr'], 'vector(0)')
        self.run_action('cleanup'); self.run_action('cleanup')
        self.assertEqual(self.api.rules, [self.api.original])
        self.assertEqual([x[0] for x in self.api.mutations()], ['POST','PUT','DELETE'])
        self.assertEqual(c.load_state(self.state)['phase'], 'cleaned')
        with self.assertRaises(ValueError): self.run_action('fire')

    def test_no_adoption_of_same_uid_even_if_other_identity_looks_similar(self):
        self.api.rules.append(c.desired_rule('a'*32, True))
        with self.assertRaises(ValueError): self.run_action()
        self.assertEqual(self.api.mutations(), []); self.assertFalse(self.state.exists())

    def test_server_snake_case_keep_firing_default_is_checked(self):
        self.run_action()
        self.api.rules[-1]['keep_firing_for'] = '0s'
        self.run_action()
        self.assertEqual(len(self.api.mutations()), 1)
        self.api.rules[-1]['keep_firing_for'] = '5m'
        with self.assertRaises(ValueError):
            self.run_action('resolve')

    def test_nonisolated_group_refused_before_creation(self):
        self.api.rules.append({'uid':'someone-else','folderUID':c.FOLDER,'ruleGroup':c.GROUP})
        with self.assertRaises(ValueError): self.run_action()
        self.assertEqual(self.api.mutations(), []); self.assertFalse(self.state.exists())

    def test_mutated_rule_is_not_resolved_or_deleted(self):
        for change in ['title','expr','labels','notifications','keepfiring','owner']:
            with self.subTest(change=change):
                state = {'schema':1,'base':c.BASE,'uid':c.UID,'folder':c.FOLDER,'datasource':c.DATASOURCE,'group':c.GROUP,'owner':'a'*32,'phase':'firing'}
                c.save_state(self.state,state); altered = c.desired_rule('a'*32, True)
                if change=='title': altered['title']='Other'
                if change=='expr': altered['data'][0]['model']['expr']='up'
                if change=='labels': altered['labels']['product']='Other'
                if change=='notifications': altered['notification_settings']={'receiver':'Feishu'}
                if change=='keepfiring': altered['keepFiringFor']='5m'
                if change=='owner': altered['annotations'][c.OWNER]='b'*32
                self.api.rules = [self.api.original, altered]; self.api.calls=[]
                with self.assertRaises(ValueError): self.run_action('resolve')
                with self.assertRaises(ValueError): self.run_action('cleanup')
                self.assertEqual(self.api.mutations(), [])

    def test_cleanup_requires_explicit_resolve(self):
        self.run_action(); self.api.calls=[]
        with self.assertRaises(ValueError): self.run_action('cleanup')
        self.assertEqual(self.api.mutations(), [])

    def test_concurrent_edit_refused_and_pending_post_can_be_recovered(self):
        base=self.api
        count=0
        def edited(method,path,body=None):
            nonlocal count
            count+=1
            if count==2:
                base.rules.append(c.desired_rule('b'*32, True))
            return base(method,path,body)
        with patch.object(c,'request',edited), self.assertRaises(ValueError): self.run_action()
        self.assertEqual(base.mutations(), [])
        # 未知POST结果重跑必须核对本状态owner，不接管并发创建者。
        with self.assertRaises(ValueError): self.run_action()
        base.rules=[base.original]
        self.run_action(); self.assertEqual(base.mutations()[0][0], 'POST')

    def test_private_state_permissions_and_symlink_fail_closed(self):
        self.run_action(); self.state.chmod(0o644)
        with self.assertRaises(ValueError): self.run_action('resolve')
        self.state.chmod(0o600)
        target=Path(self.temp.name)/'other'; target.write_text('{}'); self.state.unlink(); self.state.symlink_to(target)
        with self.assertRaises(OSError): self.run_action('resolve')


class TransportTests(unittest.TestCase):
    def test_http_errors_never_expose_response_or_token(self):
        for result in [subprocess.CompletedProcess([],0,'SECRET_BODY\n401','SECRET_STDERR'),
                       subprocess.CompletedProcess([],1,'SECRET_BODY','SECRET_STDERR'),
                       subprocess.CompletedProcess([],0,'SECRET_BODY\n200','')]:
            with self.subTest(code=result.returncode):
                with patch.dict(os.environ, {'GRAFANA_TOKEN':'SECRET_TOKEN'}), patch.object(c.subprocess,'run',return_value=result) as run:
                    with self.assertRaises(RuntimeError) as err: c.request('GET',c.RULES)
                    self.assertNotIn('SECRET',str(err.exception))
                    self.assertNotIn('SECRET_TOKEN',str(run.call_args.args))
                    self.assertIn('SECRET_TOKEN',run.call_args.kwargs['input'])

    def test_only_named_environment_token_is_used(self):
        with patch.dict(os.environ, {}, clear=True), patch.object(c.subprocess,'run') as run:
            with self.assertRaises(ValueError): c.request('GET',c.RULES)
            run.assert_not_called()


if __name__ == '__main__': unittest.main()
