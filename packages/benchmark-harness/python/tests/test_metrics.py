import unittest
from scos_benchmark.metrics import recall_at_k, precision_at_k, mean_reciprocal_rank, ndcg_at_k


class MetricsTests(unittest.TestCase):
    def test_metrics(self):
        exp = ["d1", "d2"]
        ret = ["d1", "d3", "d2"]
        self.assertEqual(recall_at_k(exp, ret, 1), 0.5)
        self.assertEqual(precision_at_k(exp, ret, 1), 1.0)
        self.assertEqual(mean_reciprocal_rank(exp, ret), 1.0)
        self.assertTrue(ndcg_at_k(exp, ret, 3) > 0)


if __name__ == "__main__":
    unittest.main()


class RankingIntegrityTests(unittest.TestCase):
    def test_duplicate_retrieval_uses_every_ranking_slot_for_precision(self):
        self.assertAlmostEqual(precision_at_k(["a", "b"], ["a", "a", "b"], 3), 2 / 3)
        self.assertAlmostEqual(precision_at_k(["a"], ["a", "a"], 2), 1 / 2)

    def test_duplicate_retrieval_cannot_inflate_ndcg(self):
        duplicate = ndcg_at_k(["a", "b"], ["a", "a", "b"], 3)
        distinct = ndcg_at_k(["a", "b"], ["a", "b"], 3)
        self.assertLess(duplicate, distinct)
        self.assertLess(duplicate, 1.0)
        self.assertEqual(ndcg_at_k(["a"], ["a", "a"], 2), 1.0)

    def test_duplicate_ground_truth_is_one_relevant_document(self):
        self.assertEqual(recall_at_k(["a", "a"], ["a"], 1), 1.0)
        self.assertEqual(ndcg_at_k(["a", "a"], ["a"], 2), 1.0)

    def test_nonpositive_k_cannot_return_quality(self):
        for k in (0, -1, -100):
            with self.subTest(k=k):
                self.assertEqual(recall_at_k(["a"], ["a"], k), 0.0)
                self.assertEqual(precision_at_k(["a"], ["a"], k), 0.0)
                self.assertEqual(ndcg_at_k(["a"], ["a"], k), 0.0)
