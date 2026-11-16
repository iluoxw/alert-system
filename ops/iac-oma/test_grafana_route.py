import copy
import unittest

from grafana_route import RECEIVER, RULE_IDS, desired_routes, desired_rules, policy_semantics, rule_semantics


class GrafanaRouteTests(unittest.TestCase):
    def test_parallel_routes_preserve_existing_delivery_and_isolate_canary(self):
        original = {'receiver': 'default', 'routes': [
            {'receiver': 'IAC-feishu', 'object_matchers': [['product', '=', 'IAC']]},
            {'receiver': 'other-team', 'object_matchers': [['product', '=', 'OTHER']]},
        ]}
        before = copy.deepcopy(original)
        updated = desired_routes(original)
        self.assertEqual(original, before)
        self.assertEqual(updated['routes'][2:], before['routes'])
        self.assertFalse(updated['routes'][0]['continue'])
        self.assertIn(['sre.canary', '=', 'true'], updated['routes'][0]['object_matchers'])
        self.assertTrue(updated['routes'][1]['continue'])
        self.assertEqual(updated['routes'][1]['receiver'], RECEIVER)
        self.assertEqual(desired_routes(updated), updated)

    def test_only_stable_labels_change(self):
        original = [{'uid': uid, 'labels': {'product': 'IAC'}, 'isPaused': False,
                     'data': [{'query': 'do-not-change'}], 'for': '5m',
                     'noDataState': 'NoData', 'execErrState': 'Error'} for uid in RULE_IDS]
        before = copy.deepcopy(original)
        pairs = desired_rules(original)
        self.assertEqual(original, before)
        self.assertEqual(len(pairs), 10)
        for old, new in pairs:
            labels = new.pop('labels')
            self.assertEqual(labels, {'product': 'IAC', 'sre.rule.id': old['uid']})
            self.assertEqual(new, {k: v for k, v in old.items() if k != 'labels'})

    def test_root_fallback_cannot_be_silently_replaced(self):
        with self.assertRaises(ValueError):
            desired_routes({'receiver': 'IAC-feishu'})
        with self.assertRaises(ValueError):
            desired_routes({'receiver': 'IAC-feishu', 'routes': [
                {'receiver': 'other', 'object_matchers': [['product', '=', 'OTHER']]},
            ]})

    def test_readback_omitted_false_is_not_an_extra_notification(self):
        expected = {'receiver': 'default', 'routes': [{'receiver': RECEIVER, 'continue': False}]}
        actual = {'receiver': 'default', 'provenance': 'api', 'routes': [{'receiver': RECEIVER}]}
        self.assertEqual(policy_semantics(expected), policy_semantics(actual))
        actual['routes'][0]['continue'] = True
        self.assertNotEqual(policy_semantics(expected), policy_semantics(actual))

    def test_rule_readback_detects_query_threshold_and_pause_drift(self):
        expected = {'uid': 'iac-rule', 'data': [{'expr': 'original'}], 'condition': 'C',
                    'for': '5m', 'isPaused': False, 'labels': {'sre.rule.id': 'iac-rule'}}
        actual = dict(expected, updated='server-time', provenance='api')
        self.assertEqual(rule_semantics(expected), rule_semantics(actual))
        for key, value in [('data', [{'expr': 'changed'}]), ('condition', 'D'),
                           ('for', '0s'), ('isPaused', True)]:
            with self.subTest(key=key):
                self.assertNotEqual(rule_semantics(expected), rule_semantics(dict(actual, **{key: value})))

    def test_missing_or_paused_rules_stop_before_writes(self):
        with self.assertRaises(ValueError):
            desired_rules([])
        rules = [{'uid': uid, 'labels': {'product': 'IAC'}, 'isPaused': False} for uid in RULE_IDS]
        rules[0]['isPaused'] = True
        with self.assertRaises(ValueError):
            desired_rules(rules)


if __name__ == '__main__':
    unittest.main()
