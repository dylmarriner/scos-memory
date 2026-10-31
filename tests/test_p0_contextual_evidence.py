"""P0: never turn unmeasured tenant isolation into a passing retrieval score."""
import pathlib
import sys
import tempfile
import json
import unittest
from unittest.mock import patch

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from benchmarks.harness import (BenchmarkCase, BenchmarkSuite, BenchmarkRunner,
                                GatewayMemoryAdapter, load_suite, main)


class ContextualEvidenceTests(unittest.TestCase):
    def test_forbidden_hit_disqualifies_task_even_with_all_expected(self):
        case = BenchmarkCase(id='isolation', query='policy', category='permissions',
                             expected_ids=['allowed'], forbidden_ids=['foreign'])
        suite = BenchmarkSuite(name='scope', version='1', cases=[case])
        result = BenchmarkRunner(suite).run(lambda query, ctx: ['allowed', 'foreign'])[0]
        self.assertFalse(result.answer_correct)
        self.assertEqual(result.scores['forbidden_at_5'], 1.0)
        self.assertEqual(result.scores['forbidden_at_10'], 1.0)
        self.assertEqual(result.scores['forbidden_at_20'], 1.0)

    def test_forbidden_leak_is_a_regression_not_an_improvement(self):
        from benchmarks.harness import compare_baseline
        case = BenchmarkCase(id='scope', query='q', category='permissions',
                             expected_ids=['allowed'], forbidden_ids=['foreign'])
        suite = BenchmarkSuite(name='scope', version='1', cases=[case])
        before = BenchmarkRunner(suite).run(lambda q, c: ['allowed'])
        after = BenchmarkRunner(suite).run(lambda q, c: ['allowed', 'foreign'])
        self.assertEqual(compare_baseline(after, before)['forbidden_at_5']['regressions'], 1)

    def test_forbidden_after_top_twenty_still_invalidates_task_success(self):
        case = BenchmarkCase(id='scope', query='q', category='permissions',
                             expected_ids=['allowed'], forbidden_ids=['foreign'])
        suite = BenchmarkSuite(name='scope', version='1', cases=[case])
        results = ['allowed'] + [f'noise-{i}' for i in range(20)] + ['foreign']
        result = BenchmarkRunner(suite).run(lambda q, c: results)[0]
        self.assertFalse(result.answer_correct)
        self.assertEqual(result.scores['forbidden_at_20'], 0.0)

    def test_no_forbidden_evidence_preserves_success(self):
        case = BenchmarkCase(id='isolation', query='policy', category='permissions',
                             expected_ids=['allowed'], forbidden_ids=['foreign'])
        suite = BenchmarkSuite(name='scope', version='1', cases=[case])
        result = BenchmarkRunner(suite).run(lambda query, ctx: ['allowed'])[0]
        self.assertTrue(result.answer_correct)
        self.assertEqual(result.scores['forbidden_at_5'], 0.0)

    def test_unlabelled_cases_do_not_claim_forbidden_coverage(self):
        case = BenchmarkCase(id='ordinary', query='policy', category='known_answer',
                             expected_ids=['allowed'])
        result = BenchmarkRunner(BenchmarkSuite(name='ordinary', version='1', cases=[case])).run(
            lambda query, ctx: ['allowed', 'foreign'])[0]
        self.assertNotIn('forbidden_at_5', result.scores)

    def test_forbidden_ids_validated_and_cannot_overlap_expected(self):
        payload = {'name':'scope','version':'1','corpus':{'allowed':'ok','foreign':'secret'},
                   'cases':[{'id':'scope','query':'ok','category':'permissions',
                             'expected_ids':['allowed'],'forbidden_ids':['foreign']}]}
        with tempfile.TemporaryDirectory() as td:
            path = pathlib.Path(td)/'suite.json'
            path.write_text(json.dumps(payload))
            self.assertEqual(load_suite(str(path)).cases[0].forbidden_ids, ['foreign'])
            for bad in (['allowed'], ['foreign','foreign'], ['unknown'], 'foreign', [False]):
                with self.subTest(bad=bad):
                    payload['cases'][0]['forbidden_ids'] = bad
                    path.write_text(json.dumps(payload))
                    with self.assertRaises(ValueError):
                        load_suite(str(path))

    def test_gateway_rejects_unsupported_tenant_scope_before_network(self):
        adapter = GatewayMemoryAdapter('https://example.invalid/mcp', {'allowed':'ok'})
        with patch.object(adapter, '_call') as call:
            with self.assertRaisesRegex(ValueError, 'context.*unsupported'):
                adapter('policy', {'tenant_id':'tenant-alpha'})
            call.assert_not_called()

    def test_main_rejects_contextual_suite_before_seeding(self):
        payload = {'name':'scope','version':'1','corpus':{'allowed':'ok'},
                   'cases':[{'id':'scope','query':'ok','category':'permissions',
                             'expected_ids':['allowed'],'context':{'tenant_id':'tenant-alpha'}}]}
        with tempfile.TemporaryDirectory() as td:
            path = pathlib.Path(td)/'suite.json'
            path.write_text(json.dumps(payload))
            with patch.object(GatewayMemoryAdapter, '__enter__', side_effect=AssertionError('seeded')):
                with self.assertRaisesRegex(ValueError, 'context.*unsupported'):
                    main(['--suite',str(path),'--endpoint','https://example.invalid/mcp'])


if __name__ == '__main__':
    unittest.main()
