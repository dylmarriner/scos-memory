"""Tests for benchmarks/harness.py against GOVERNANCE.md §33.

§33 requires the versioned evaluation suite to cover ten named case
categories and to track Recall@K, Precision@K, MRR, nDCG, citation
accuracy, authority accuracy, conflict detection, duplicate rate,
stale-hit rate, promotion precision/recall, token efficiency and answer
regression. These tests guard against silently losing any of them.
"""
import pathlib
import sys
import unittest

ROOT = pathlib.Path(__file__).parents[1]
sys.path.insert(0, str(ROOT))

from benchmarks.harness import (  # noqa: E402
    BenchmarkCase,
    BenchmarkResult,
    BenchmarkRunner,
    DeterministicLexicalBaseline,
    answer_regression,
    authority_accuracy,
    citation_accuracy,
    compute_scores,
    conflict_detection,
    create_default_frozen_suite,
    duplicate_rate,
    promotion_precision_recall,
    stale_hit_rate,
    token_efficiency,
)

REQUIRED_CATEGORIES = {
    "known_answer",
    "exact_identifier",
    "temporal",
    "contradiction",
    "permissions",
    "project_isolation",
    "multi_turn",
    "recovery",
    "stale",
    "prompt_injection",
}

REQUIRED_SUMMARY_METRICS = {
    "recall@1", "precision@1", "mrr", "ndcg@5",
    "citation_accuracy", "authority_accuracy", "conflict_detection",
    "duplicate_rate", "stale_hit_rate",
    "promotion_precision", "promotion_recall",
    "answer_regression",
}


class DefaultSuiteCoversAllCategoriesTests(unittest.TestCase):
    def test_all_ten_categories_present(self):
        suite = create_default_frozen_suite()
        categories = {case.category for case in suite.cases}
        self.assertEqual(categories, REQUIRED_CATEGORIES)

    def test_project_isolation_is_distinct_from_permissions(self):
        suite = create_default_frozen_suite()
        by_category = {}
        for case in suite.cases:
            by_category.setdefault(case.category, []).append(case)
        permissions_ids = {i for c in by_category["permissions"] for i in c.expected_ids}
        isolation_ids = {i for c in by_category["project_isolation"] for i in c.expected_ids}
        self.assertTrue(permissions_ids.isdisjoint(isolation_ids))

    def test_summary_reports_all_required_metrics(self):
        suite = create_default_frozen_suite()
        runner = BenchmarkRunner(suite)
        runner.run(DeterministicLexicalBaseline())
        summary = runner.summary()
        self.assertTrue(REQUIRED_SUMMARY_METRICS.issubset(summary["average_scores"].keys()))
        self.assertEqual(set(summary["categories"]), REQUIRED_CATEGORIES)


class MetricFunctionTests(unittest.TestCase):
    def test_citation_accuracy_none_when_no_assertion(self):
        self.assertIsNone(citation_accuracy([], ["a"]))

    def test_citation_accuracy_scores_correct_and_incorrect_citations(self):
        self.assertEqual(citation_accuracy(["a"], ["a"]), 1.0)
        self.assertEqual(citation_accuracy(["a"], ["b"]), 0.0)

    def test_authority_accuracy_measures_top_k_authority_share(self):
        self.assertEqual(authority_accuracy(["a"], ["a", "b"], 2), 0.5)
        self.assertIsNone(authority_accuracy([], ["a"], 2))

    def test_conflict_detection_flags_both_sides_surfaced(self):
        self.assertEqual(conflict_detection(["a", "b"], ["a", "b"]), 0.0)
        self.assertEqual(conflict_detection(["a", "b"], ["a"]), 1.0)

    def test_duplicate_and_stale_rates(self):
        self.assertEqual(duplicate_rate(["dup"], ["dup", "real"]), 0.5)
        self.assertEqual(stale_hit_rate(["old"], ["old", "real"]), 0.5)
        self.assertIsNone(duplicate_rate([], ["real"]))
        self.assertIsNone(stale_hit_rate([], ["real"]))

    def test_token_efficiency_capped_at_one(self):
        self.assertEqual(token_efficiency(100, 50), 1.0)
        self.assertEqual(token_efficiency(100, 200), 0.5)
        self.assertIsNone(token_efficiency(0, 50))

    def test_answer_regression_only_when_baseline_correct_and_current_is_not(self):
        self.assertEqual(answer_regression(True, False), 1.0)
        self.assertEqual(answer_regression(True, True), 0.0)
        self.assertEqual(answer_regression(False, False), 0.0)

    def test_promotion_precision_recall(self):
        cases = [
            BenchmarkCase(id="c1", query="q", expected_ids=[], category="known_answer", promotion_expected=True),
            BenchmarkCase(id="c2", query="q", expected_ids=[], category="known_answer", promotion_expected=False),
        ]
        results = [
            BenchmarkResult(case_id="c1", retrieved_ids=[], latency_ms=0, promoted=True),
            BenchmarkResult(case_id="c2", retrieved_ids=[], latency_ms=0, promoted=True),
        ]
        stats = promotion_precision_recall(cases, results)
        self.assertEqual(stats["promotion_precision"], 0.5)
        self.assertEqual(stats["promotion_recall"], 1.0)

    def test_compute_scores_without_case_or_result_is_base_metrics_only(self):
        scores = compute_scores({"a"}, ["a"])
        self.assertNotIn("citation_accuracy", scores)
        self.assertIn("recall@1", scores)


if __name__ == "__main__":
    unittest.main()
