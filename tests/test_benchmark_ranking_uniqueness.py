"""Ranking slots must not receive multiple relevance rewards for one item."""
import unittest

from benchmarks.harness import (
    BenchmarkCase, BenchmarkResult, authority_accuracy, compute_scores, ndcg, precision_at_k,
)


class RankingDuplicateIntegrityTests(unittest.TestCase):
    def test_repeated_expected_id_uses_distinct_precision_denominator_slots(self):
        # Two unique relevant records occupy three returned slots.
        self.assertAlmostEqual(
            precision_at_k({"a", "b"}, ["a", "a", "b"], 3), 2 / 3
        )

    def test_ndcg_never_rewards_duplicate_relevant_evidence_twice(self):
        # An adversarial retriever repeats its best match in place of
        # presenting another distinct source.
        self.assertEqual(ndcg({"a"}, ["a", "a"], 5), 1.0)
        self.assertLessEqual(ndcg({"a", "b"}, ["a", "a", "b"], 3), 1.0)
        self.assertLess(ndcg({"a", "b"}, ["a", "a", "b"], 3),
                        ndcg({"a", "b"}, ["a", "b"], 3))

    def test_duplicate_authoritative_id_cannot_earn_multiple_authority_hits(self):
        # Repeating one trusted source must not make a low-diversity result
        # appear more authoritative than its distinct evidence warrants.
        self.assertAlmostEqual(authority_accuracy(["a"], ["a", "a", "x"], 3), 1 / 3)
        self.assertAlmostEqual(authority_accuracy(["a", "b"], ["a", "a", "b"], 3), 2 / 3)

    def test_duplicate_authority_is_not_inflated_in_complete_score_bundle(self):
        case = BenchmarkCase(id="authority-dup", query="q", expected_ids=["a"],
                             category="known_answer", authoritative_ids=["a"])
        result = BenchmarkResult(case_id="authority-dup", retrieved_ids=["a", "a", "x"], latency_ms=1.0)
        scores = compute_scores({"a"}, result.retrieved_ids, case, result)
        self.assertAlmostEqual(scores["authority_accuracy"], 1 / 3)

        scores = compute_scores({"a", "b"}, ["a", "a", "b"])
        self.assertLess(scores["precision@3"], 1)
        self.assertLess(scores["ndcg@5"], 1)
        self.assertGreaterEqual(scores["ndcg@5"], 0)


if __name__ == "__main__":
    unittest.main()
