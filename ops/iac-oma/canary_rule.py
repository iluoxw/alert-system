#!/usr/bin/env python3
"""独立 Grafana canary；默认只读。必须先核对排除飞书的 canary 路由。

GRAFANA_TOKEN 仅取环境变量。示例：
  python3 ops/iac-oma/canary_rule.py --state-file /private/tmp/iac-canary.json
  # 审阅 plan 与路由后，用 --apply 创建并 firing；随后 --action resolve --apply
  # 等 resolved 送达后，再 --action cleanup --apply 删除规则。
"""
import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
import uuid

BASE = 'https://e2b-grafana.test.seewo.com'
FOLDER = 'cfyf3iaiy169sa'
DATASOURCE = 'ffsel4wyd1q80f'
UID = 'iac-sre-oma-canary'
TITLE = 'IAC-SRE-OMA-Canary'
GROUP = 'IAC SRE OMA isolated canary'
RULES = '/api/v1/provisioning/alert-rules'
OWNER = 'sre.canary.owner'
MANAGED = 'iac-oma-canary-v1'


def request(method, path, body=None):
    token = os.environ.get('GRAFANA_TOKEN', '')
    if not token or any(c in token for c in '\r\n'):
        raise ValueError('GRAFANA_TOKEN must be supplied through environment')
    if path not in (RULES, RULES + '/' + UID):
        raise ValueError('API path is outside canary scope')
    config = ['url = ' + json.dumps(BASE + path), 'request = ' + json.dumps(method),
              'header = ' + json.dumps('Authorization: Bearer ' + token)]
    if body is not None:
        config += ['header = "Content-Type: application/json"',
                   'data = ' + json.dumps(json.dumps(body))]
    try:
        p = subprocess.run(['curl', '-sS', '--proto', '=https', '--max-time', '25',
                            '--max-filesize', '2097152', '--write-out', '\n%{http_code}',
                            '--config', '-'], input='\n'.join(config), text=True,
                           capture_output=True, timeout=30)
    except (subprocess.TimeoutExpired, OSError):
        raise RuntimeError('Grafana transport failed; response suppressed') from None
    if p.returncode:
        raise RuntimeError('Grafana transport failed; response suppressed')
    raw, _, code = p.stdout.rpartition('\n')
    if not code.isdigit() or not 200 <= int(code) < 300:
        raise RuntimeError('Grafana HTTP request failed; response suppressed')
    try:
        return json.loads(raw) if raw else None
    except (ValueError, TypeError):
        raise RuntimeError('Grafana returned invalid JSON; response suppressed') from None


def desired_rule(owner, firing):
    return {'uid': UID, 'title': TITLE, 'folderUID': FOLDER, 'ruleGroup': GROUP,
            'condition': 'C', 'for': '0s', 'noDataState': 'NoData', 'execErrState': 'Error',
            'isPaused': False,
            'labels': {'product': 'IAC', 'sre.rule.id': UID, 'sre.canary': 'true'},
            'annotations': {'summary': TITLE, 'sre.canary.managed_by': MANAGED, OWNER: owner},
            'data': [{'refId': 'A', 'queryType': '',
                      'relativeTimeRange': {'from': 60, 'to': 0}, 'datasourceUid': DATASOURCE,
                      'model': {'refId': 'A', 'datasource': {'type': 'prometheus', 'uid': DATASOURCE},
                                'expr': 'vector(1)' if firing else 'vector(0)', 'instant': True,
                                'range': False, 'intervalMs': 1000, 'maxDataPoints': 43200}},
                     {'refId': 'C', 'queryType': '', 'relativeTimeRange': {'from': 0, 'to': 0},
                      'datasourceUid': '__expr__',
                      'model': {'refId': 'C', 'type': 'threshold', 'expression': 'A',
                                'datasource': {'type': '__expr__', 'uid': '__expr__'},
                                'intervalMs': 1000, 'maxDataPoints': 43200,
                                'conditions': [{'evaluator': {'params': [0], 'type': 'gt'},
                                                'operator': {'type': 'and'}, 'query': {'params': ['C']},
                                                'reducer': {'type': 'last', 'params': []}, 'type': 'query'}]}}]}


def check_rule(rule, owner):
    # 服务端可增加 id/orgID/provenance/updated；所有受控语义字段必须逐项相同。
    allowed_extra = {'id', 'orgID', 'updated', 'provenance', 'keepFiringFor', 'notification_settings', 'record'}
    if (set(rule) - set(desired_rule(owner, True)) - allowed_extra
            or rule.get('keepFiringFor') not in (None, '', '0s')
            or rule.get('notification_settings') not in (None, {})
            or rule.get('record') not in (None, {})):
        raise ValueError('Unexpected canary semantics; refusing mutation')
    for firing in (True, False):
        expected = desired_rule(owner, firing)
        if all(rule.get(k) == v for k, v in expected.items()):
            return firing
    raise ValueError('Existing canary differs from owned fixed definition; refusing mutation')


def inventory():
    rules = request('GET', RULES)
    if not isinstance(rules, list) or any(not isinstance(r, dict) for r in rules):
        raise ValueError('Unexpected Grafana inventory schema')
    matches = [r for r in rules if r.get('uid') == UID]
    group = [r for r in rules if r.get('folderUID') == FOLDER and r.get('ruleGroup') == GROUP]
    if len(matches) > 1 or any(r.get('uid') != UID for r in group):
        raise ValueError('Canary UID/group is not isolated; refusing mutation')
    return matches[0] if matches else None


def load_state(path):
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    except FileNotFoundError:
        return None
    with os.fdopen(fd) as f:
        st = os.fstat(f.fileno())
        if not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600 or st.st_uid != os.getuid() or st.st_size > 4096:
            raise ValueError('State must be an owned regular 0600 file')
        try:
            value = json.load(f)
        except ValueError:
            raise ValueError('Invalid canary state') from None
    expected = {'schema': 1, 'base': BASE, 'uid': UID, 'folder': FOLDER, 'datasource': DATASOURCE, 'group': GROUP}
    if (not isinstance(value, dict) or any(value.get(k) != v for k, v in expected.items())
            or not re.fullmatch(r'[0-9a-f]{32}', value.get('owner', ''))
            or value.get('phase') not in ('creating', 'firing', 'resolved', 'cleaned')):
        raise ValueError('State identity mismatch')
    return value


def save_state(path, state):
    if path.is_symlink():
        raise ValueError('Symlink state refused')
    with tempfile.NamedTemporaryFile(mode='w', dir=path.parent, prefix='.iac-canary-', delete=False) as f:
        temp = Path(f.name)
        try:
            os.fchmod(f.fileno(), 0o600)
            json.dump(state, f, indent=2); f.write('\n'); f.flush(); os.fsync(f.fileno())
            os.replace(temp, path)
        finally:
            temp.unlink(missing_ok=True)


@contextlib.contextmanager
def exclusive(path, apply):
    if not apply:
        yield
        return
    # 只在显式 apply 写锁文件；同一状态文件的两个操作不能交叉。
    fd = os.open(str(path) + '.lock', os.O_WRONLY | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    try:
        st = os.fstat(fd)
        if not stat.S_ISREG(st.st_mode) or stat.S_IMODE(st.st_mode) != 0o600 or st.st_uid != os.getuid():
            raise ValueError('Unsafe canary lock file')
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def run(action, apply, state_file):
    with exclusive(state_file, apply):
        state = load_state(state_file)
        current = inventory()
        if current is not None:
            if state is None or state['phase'] == 'cleaned':
                raise ValueError('Preexisting rule is not owned by this state; refusing adoption')
            current_firing = check_rule(current, state['owner'])
            if action == 'cleanup' and current_firing:
                raise ValueError('Resolve and verify delivery before cleanup')
        else:
            current_firing = None
            if action == 'resolve':
                raise ValueError('Cannot resolve an absent canary')
            if action == 'fire' and state is not None and state['phase'] != 'creating':
                raise ValueError('Canary missing or state already used; inspect and use a fresh state')
        print(json.dumps({'mode': 'apply' if apply else 'plan', 'action': action, 'uid': UID,
                          'title': TITLE, 'folder': FOLDER, 'group': GROUP, 'datasource': DATASOURCE,
                          'exists': current is not None, 'current_firing': current_firing,
                          'expression': 'vector(1)' if action == 'fire' else 'vector(0)' if action == 'resolve' else None,
                          'route_precondition': 'operator must verify canary excludes Feishu before apply'}))
        if not apply:
            return
        if action == 'cleanup' and current is None:
            if state:
                state['phase'] = 'cleaned'; save_state(state_file, state)
            return
        if state is None:
            state = {'schema': 1, 'base': BASE, 'uid': UID, 'folder': FOLDER,
                     'datasource': DATASOURCE, 'group': GROUP, 'owner': uuid.uuid4().hex, 'phase': 'creating'}
            save_state(state_file, state)  # POST响应未知也留下可核对的所有权；不自动重试写入。
        # 再读远端，拒绝计划后并发改变/相同group被占用。API无CAS，操作员仍需独占此canary。
        fresh = inventory()
        if fresh != current:
            raise ValueError('Concurrent canary edit; no write performed')
        if action == 'cleanup':
            check_rule(fresh, state['owner'])
            request('DELETE', RULES + '/' + UID)
            if inventory() is not None:
                raise RuntimeError('Canary deletion read-back failed')
            state['phase'] = 'cleaned'
        else:
            desired = desired_rule(state['owner'], action == 'fire')
            if fresh is None:
                request('POST', RULES, desired)
            elif current_firing != (action == 'fire'):
                # 只发本工具白名单字段，不回写服务端只读属性。
                request('PUT', RULES + '/' + UID, desired)
            actual = inventory()
            if actual is None or check_rule(actual, state['owner']) != (action == 'fire'):
                raise RuntimeError('Canary mutation read-back failed')
            state['phase'] = 'firing' if action == 'fire' else 'resolved'
        save_state(state_file, state)
        print(json.dumps({'verified': True, 'phase': state['phase'], 'only_rule_uid': UID}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--action', choices=('fire', 'resolve', 'cleanup'), default='fire')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--state-file', type=Path, required=True, help='private 0600 state outside repository')
    args = parser.parse_args()
    try:
        run(args.action, args.apply, args.state_file)
    except (ValueError, RuntimeError, OSError):
        # 不打印第三方异常字符串；可能包含路径、header或远端响应。
        raise SystemExit('Canary operation refused/failed; inspect identity and private state (details suppressed)') from None


if __name__ == '__main__':
    main()
