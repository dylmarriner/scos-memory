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

    def test_duplicates_counted_once(self):
        self.assertEqual(recall_at_k(["a"], ["a", "a"], 2), 1.0)
        self.assertEqual(precision_at_k(["a"], ["a", "a"], 2), 0.5)
        self.assertEqual(ndcg_at_k(["a"], ["a", "a"], 2), 1.0)
        self.assertLess(ndcg_at_k(["a", "b"], ["a"], 3), 1.0)


if __name__ == "__main__":
    unittest.main()
