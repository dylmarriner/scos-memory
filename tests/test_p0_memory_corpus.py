"""Versioned P0 contextual memory fixture, without pretending to run live RLS."""
import copy
import json
import pathlib
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from benchmarks.harness import BenchmarkRunner, DeterministicLexicalBaseline, load_suite

FIXTURE = ROOT / 'benchmarks/p0/memory_retrieval_v1.json'


class MemoryCorpusContractTests(unittest.TestCase):
    def setUp(self):
        self.data = json.loads(FIXTURE.read_text())

    def load(self, data):
        with tempfile.TemporaryDirectory() as td:
            path = pathlib.Path(td) / 'suite.json'
            path.write_text(json.dumps(data))
            return load_suite(str(path))

    def test_versioned_fixture_covers_nine_categories_and_scoped_labels(self):
        suite = load_suite(str(FIXTURE))
        self.assertEqual(suite.version, '1.0.0')
        self.assertEqual(len(suite.corpus), 43)
        self.assertEqual(len(suite.cases), 30)
        self.assertEqual(len(suite.document_scopes), 43)
        self.assertGreaterEqual(len({c.category for c in suite.cases}), 9)
        self.assertTrue(all(c.context.get('tenant_id') and c.context.get('namespace_id') for c in suite.cases))
        self.assertGreaterEqual(sum(bool(c.forbidden_ids) for c in suite.cases), 24)
        self.assertEqual(len({v['tenant_id'] for v in suite.document_scopes.values()}), 2)

    def test_scope_metadata_must_cover_exact_corpus(self):
        for mutate in (lambda d: d['document_scopes'].pop('alpha_db'),
                       lambda d: d['document_scopes'].update(ghost=d['document_scopes']['alpha_db']),
                       lambda d: d['document_scopes']['alpha_db'].update(tenant_id='invalid'),
                       lambda d: d['document_scopes']['alpha_db'].update(state='verified'),
                       lambda d: d['document_scopes']['alpha_db'].update(state=[])):
            with self.subTest(mutate=mutate):
                data = copy.deepcopy(self.data)
                mutate(data)
                with self.assertRaises(ValueError):
                    self.load(data)

    def test_expected_evidence_cannot_belong_to_other_tenant_or_namespace(self):
        for id in ('beta_db', 'alpha_finance_rate'):
            with self.subTest(id=id):
                data = copy.deepcopy(self.data)
                data['cases'][0]['expected_ids'] = [id]
                data['cases'][0]['forbidden_ids'] = []
                with self.assertRaisesRegex(ValueError, 'scope'):
                    self.load(data)

    def test_temporal_historical_labels_require_explicit_time(self):
        data = copy.deepcopy(self.data)
        case = next(c for c in data['cases'] if c['id']=='temporal_2024')
        case['context'].pop('as_of_valid_time')
        with self.assertRaisesRegex(ValueError, 'historical'):
            self.load(data)

    def test_invalid_or_unsupported_temporal_context_is_rejected(self):
        for invalid in (['2024-05-01'], 'yesterday', '2024-13-01T00:00:00Z',
                        '2024-05-01T00:00:00', '2024-05-01T00:00:00+24:00'):
            with self.subTest(invalid=invalid):
                data = copy.deepcopy(self.data)
                case = next(c for c in data['cases'] if c['id'] == 'temporal_2024')
                case['context']['as_of_valid_time'] = invalid
                with self.assertRaisesRegex(ValueError, 'valid time'):
                    self.load(data)
        data = copy.deepcopy(self.data)
        data['cases'][0]['context']['as_of_system_time'] = '2024-05-01T00:00:00Z'
        with self.assertRaisesRegex(ValueError, 'context'):
            self.load(data)

    def test_isolation_cases_require_out_of_scope_forbidden_evidence(self):
        data = copy.deepcopy(self.data)
        case = next(c for c in data['cases'] if c['id']=='tenant_release')
        case['forbidden_ids'] = ['alpha_release_old']
        with self.assertRaisesRegex(ValueError, 'isolation'):
            self.load(data)

    def test_namespace_isolation_requires_same_tenant_other_namespace(self):
        data = copy.deepcopy(self.data)
        case = next(c for c in data['cases'] if c['id'] == 'namespace_budget')
        case['forbidden_ids'] = ['beta_finance']
        with self.assertRaisesRegex(ValueError, 'namespace isolation'):
            self.load(data)

    def test_lexical_baseline_is_diagnostic_not_scope_qualification(self):
        suite = load_suite(str(FIXTURE))
        runner = BenchmarkRunner(suite)
        results = runner.run(DeterministicLexicalBaseline(suite.corpus))
        self.assertEqual(len(results), 30)
        self.assertIn('forbidden_at_5', runner.summary()['average_scores'])
        self.assertTrue(any(not result.answer_correct for result in results))


if __name__ == '__main__':
    unittest.main()
