import unittest
from main import evaluate_policy_candidate


class PolicyEvalTests(unittest.TestCase):
    def test_evaluate_policy_candidate(self):
        res = evaluate_policy_candidate("test_policy", ["t1", "t2"])
        self.assertEqual(res["eval_status"], "passed")
        self.assertEqual(res["trajectories_evaluated"], 2)


if __name__ == "__main__":
    unittest.main()
