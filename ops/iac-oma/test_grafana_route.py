import copy
import unittest

from grafana_route import RECEIVER, RULE_IDS, desired_routes, desired_rules


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

    def test_missing_or_paused_rules_stop_before_writes(self):
        with self.assertRaises(ValueError):
            desired_rules([])
        rules = [{'uid': uid, 'labels': {'product': 'IAC'}, 'isPaused': False} for uid in RULE_IDS]
        rules[0]['isPaused'] = True
        with self.assertRaises(ValueError):
            desired_rules(rules)


if __name__ == '__main__':
    unittest.main()
