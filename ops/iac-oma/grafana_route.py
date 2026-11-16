#!/usr/bin/env python3
"""Configure IAC's parallel SRE receiver. Default is a read-only plan.

GRAFANA_TOKEN and SRE_CONTROL_WEBHOOK_URL are runtime secrets; never pass them
on argv. Store private backups outside the repository with --state-dir.
"""
import argparse
import copy
import json
import os
from pathlib import Path
import re
import subprocess
from urllib.parse import urlparse

BASE = 'https://e2b-grafana.test.seewo.com'
UID = 'iac-sre-control-webhook'
RECEIVER = 'IAC-SRE-Control'
RULE_IDS = (
    'iac-go-system-errors', 'iac-go-latency-p99', 'iac-go-traffic-zero',
    'iac-go-metrics-stale', 'iac-go-commit-unknown', 'iac-go-outbox-dead',
    'iac-go-outbox-lag', 'iac-api-probe-not-all-200',
    'iac-api-probe-delay-high', 'iac-redis002-vip-ras-down',
)


def request(method, path, body=None, *, editable=False):
    token = os.environ.get('GRAFANA_TOKEN')
    if not token:
        raise ValueError('GRAFANA_TOKEN is required')
    assert path.startswith('/api/') and '://' not in path
    config = ['url = ' + json.dumps(BASE + path),
              'request = ' + json.dumps(method),
              'header = ' + json.dumps('Authorization: Bearer ' + token)]
    if editable:
        config.append('header = "X-Disable-Provenance: true"')
    if body is not None:
        config += ['header = "Content-Type: application/json"',
                   'data = ' + json.dumps(json.dumps(body))]
    result = subprocess.run(
        ['curl', '-sS', '--fail-with-body', '--max-time', '40', '--config', '-'],
        input='\n'.join(config), text=True, capture_output=True, timeout=45)
    if result.returncode:
        # Error bodies may echo webhook credentials or existing contacts.
        raise RuntimeError(f'{method} Grafana API failed (curl {result.returncode}); inspect private state')
    return json.loads(result.stdout) if result.stdout else None


def save(root, name, value):
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(root, 0o700)
    path = root / name
    data = json.dumps(value, ensure_ascii=False, indent=2)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as stream:
        stream.write(data)


def desired_routes(policies):
    desired = copy.deepcopy(policies)
    assert policies.get('receiver'), 'Missing root receiver'
    original = [r for r in policies.get('routes', []) if r.get('receiver') != RECEIVER]
    # Matching a new child prevents fallback to the root receiver, even with
    # continue=true. Require the existing explicit IAC path before any writes.
    if not any(r.get('receiver') and r.get('object_matchers') == [['product', '=', 'IAC']]
               for r in original):
        raise ValueError('An existing explicit product=IAC notification route is required')
    # Canary must stop before the preserved real Feishu routes.
    common = {'receiver': RECEIVER, 'group_by': ['alertname', 'severity'],
              'group_interval': '5m', 'repeat_interval': '2h'}
    canary = dict(common, object_matchers=[['product', '=', 'IAC'], ['sre.canary', '=', 'true']],
                  group_wait='1s', **{'continue': False})
    normal = dict(common, object_matchers=[['product', '=', 'IAC']],
                  group_wait='30s', **{'continue': True})
    desired['routes'] = [canary, normal] + original
    return desired


def policy_semantics(policy):
    """Grafana omits false continuation and adds provisioning metadata."""
    result = copy.deepcopy(policy)
    result.pop('provenance', None)
    result['continue'] = result.get('continue', False)
    if 'routes' in result:
        result['routes'] = [policy_semantics(r) for r in result['routes']]
    return result


def rule_semantics(rule):
    return {key: value for key, value in rule.items()
            if key not in ('updated', 'provenance')}


def desired_rules(rules):
    by_id = {r['uid']: r for r in rules}
    updates = []
    for uid in RULE_IDS:
        if uid not in by_id:
            raise ValueError('Missing expected IAC rule: ' + uid)
        old = by_id[uid]
        if old.get('isPaused') or old.get('labels', {}).get('product') != 'IAC':
            raise ValueError('Unexpected rule state/product: ' + uid)
        new = copy.deepcopy(old)
        new.setdefault('labels', {})['sre.rule.id'] = uid
        if new != old:
            updates.append((old, new))
    return updates


def contact_url():
    url = os.environ.get('SRE_CONTROL_WEBHOOK_URL', '')
    parsed = urlparse(url)
    if (parsed.scheme != 'https' or parsed.hostname != 'sre-control.test.seewo.com'
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or not re.fullmatch(r'/ingest/v1/hook/[0-9a-f-]{36}', parsed.path)):
        raise ValueError('A verified active Grafana Source webhook is required')
    return url


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--state-dir', type=Path, required=True)
    args = parser.parse_args()
    os.umask(0o077)
    policies = request('GET', '/api/v1/provisioning/policies')
    contacts = request('GET', '/api/v1/provisioning/contact-points')
    rules = request('GET', '/api/v1/provisioning/alert-rules')
    updated = desired_routes(policies)
    policy_change = policy_semantics(updated) != policy_semantics(policies)
    changes = desired_rules(rules)
    desired_contact = {'uid': UID, 'name': RECEIVER, 'type': 'webhook',
                       'disableResolveMessage': False,
                       'settings': {'url': contact_url(), 'httpMethod': 'POST', 'maxAlerts': '0'}}
    matches = [c for c in contacts if c.get('uid') == UID or c.get('name') == RECEIVER]
    if matches and (len(matches) != 1 or matches[0]['uid'] != UID
                    or matches[0].get('settings') != desired_contact['settings']
                    or matches[0].get('type') != 'webhook'
                    or matches[0].get('disableResolveMessage', False)):
        raise ValueError('Existing SRE contact differs; inspect before replacement')
    print(json.dumps({'mode': 'apply' if args.apply else 'plan',
                      'rule_label_updates': [v[0]['uid'] for v in changes],
                      'create_contact': not matches, 'policy_change': policy_change,
                      'preserved_routes': len([r for r in policies.get('routes', [])
                                               if r.get('receiver') != RECEIVER])}))
    if not args.apply:
        return
    # A fresh directory prevents accidental overwrite of the only rollback copy.
    save(args.state_dir, 'before.json', {'policies': policies, 'contacts': contacts,
                                       'rules': [r for r in rules if r.get('uid') in RULE_IDS]})
    for old, new in changes:
        path = '/api/v1/provisioning/alert-rules/' + old['uid']
        if request('GET', path) != old:
            raise RuntimeError('Concurrent rule edit: ' + old['uid'])
        request('PUT', path, new, editable=not old.get('provenance'))
        actual = request('GET', path)
        if (rule_semantics(actual) != rule_semantics(new)
                or (actual.get('provenance') or '') != (old.get('provenance') or '')):
            raise RuntimeError('Rule label verification failed: ' + old['uid'])
        save(args.state_dir, old['uid'] + '.applied.json', {'uid': old['uid'], 'labels': actual['labels']})
    if request('GET', '/api/v1/provisioning/policies') != policies:
        raise RuntimeError('Concurrent policy edit; contact/route not changed')
    if not matches:
        request('POST', '/api/v1/provisioning/contact-points', desired_contact)
        save(args.state_dir, 'contact-created.json', {'uid': UID})
    if policy_change:
        request('PUT', '/api/v1/provisioning/policies', updated,
                editable=not policies.get('provenance'))
    actual = request('GET', '/api/v1/provisioning/policies')
    if (policy_semantics(actual) != policy_semantics(updated)
            or (actual.get('provenance') or '') != (policies.get('provenance') or '')):
        raise RuntimeError('Policy read-back differs; inspect before retry')
    actual_contacts = request('GET', '/api/v1/provisioning/contact-points')
    old_by_id = {c['uid']: c for c in contacts if c['uid'] != UID}
    new_by_id = {c['uid']: c for c in actual_contacts}
    for uid, old in old_by_id.items():
        if new_by_id.get(uid) != old:
            raise RuntimeError('Existing contact changed: ' + uid)
    if new_by_id.get(UID, {}).get('settings') != desired_contact['settings']:
        raise RuntimeError('SRE contact read-back differs')
    save(args.state_dir, 'verified.json', {'rules': list(RULE_IDS), 'contact_uid': UID,
                                         'original_contacts_unchanged': True,
                                         'original_routes_preserved': True,
                                         'canary_excludes_real_feishu': True})
    print('Applied and verified; no test alert sent.')


if __name__ == '__main__':
    main()
