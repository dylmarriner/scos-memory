import unittest
from unittest.mock import MagicMock

from main import (
    compute_success_rate,
    evaluate_policy_candidate,
    evaluate_regression,
    fetch_policy_outcomes,
    list_policy_versions,
    list_tenant_ids,
    recommend_action,
    run_evaluation_cycle,
)


def fake_cursor(rows=None):
    cur = MagicMock()
    cur.fetchall.return_value = rows or []
    return cur


class ComputeSuccessRateTests(unittest.TestCase):
    def test_all_success(self):
        self.assertEqual(compute_success_rate([{"success": True}, {"success": True}]), 1.0)

    def test_mixed(self):
        self.assertEqual(
            compute_success_rate([{"success": True}, {"success": False}, {"success": True}]),
            2 / 3,
        )

    def test_ignores_outcomes_without_success_field(self):
        self.assertEqual(
            compute_success_rate([{"success": True}, {"latency_ms": 40}]),
            1.0,
        )

    def test_empty_or_unjudged_returns_none(self):
        self.assertIsNone(compute_success_rate([]))
        self.assertIsNone(compute_success_rate([{"latency_ms": 40}]))


class EvaluateRegressionTests(unittest.TestCase):
    def test_insufficient_data_below_min_samples(self):
        self.assertEqual(
            evaluate_regression(0.9, 5, 0.9, 5, min_samples=20),
            "insufficient_data",
        )

    def test_insufficient_data_when_rate_missing(self):
        self.assertEqual(
            evaluate_regression(None, 50, 0.9, 50, min_samples=20),
            "insufficient_data",
        )

    def test_regression_detected(self):
        self.assertEqual(
            evaluate_regression(0.90, 50, 0.70, 50, min_samples=20, threshold=0.05),
            "regression_detected",
        )

    def test_improved(self):
        self.assertEqual(
            evaluate_regression(0.70, 50, 0.90, 50, min_samples=20, threshold=0.05),
            "improved",
        )

    def test_stable_within_threshold(self):
        self.assertEqual(
            evaluate_regression(0.80, 50, 0.81, 50, min_samples=20, threshold=0.05),
            "stable",
        )


class RecommendActionTests(unittest.TestCase):
    def test_mapping(self):
        self.assertEqual(recommend_action("insufficient_data"), "hold")
        self.assertEqual(recommend_action("regression_detected"), "rollback")
        self.assertEqual(recommend_action("improved"), "promote")
        self.assertEqual(recommend_action("stable"), "promote")


class DbHelperTests(unittest.TestCase):
    def test_list_tenant_ids(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(rows=[("t1",), ("t2",)])
        self.assertEqual(list_tenant_ids(conn), ["t1", "t2"])

    def test_list_policy_versions(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(rows=[("v1",), ("v2",)])
        self.assertEqual(list_policy_versions(conn, "t1"), ["v1", "v2"])

    def test_fetch_policy_outcomes(self):
        conn = MagicMock()
        conn.execute.return_value = fake_cursor(rows=[({"success": True},), ({"success": False},)])
        outcomes = fetch_policy_outcomes(conn, "t1", "v2")
        self.assertEqual(outcomes, [{"success": True}, {"success": False}])


class EvaluatePolicyCandidateTests(unittest.TestCase):
    def test_records_and_returns_rollback_on_regression(self):
        conn = MagicMock()
        baseline = [{"success": True}] * 45 + [{"success": False}] * 5  # 90%
        candidate = [{"success": True}] * 30 + [{"success": False}] * 20  # 60%
        conn.execute.side_effect = [
            fake_cursor(rows=[(o,) for o in candidate]),  # fetch candidate outcomes
            fake_cursor(rows=[(o,) for o in baseline]),  # fetch baseline outcomes
            fake_cursor(),  # INSERT audit entry
        ]
        result = evaluate_policy_candidate(conn, "t1", "v2", "v1")
        self.assertEqual(result["eval_status"], "regression_detected")
        self.assertEqual(result["recommended_action"], "rollback")
        insert_call = conn.execute.call_args_list[-1]
        self.assertIn("INSERT INTO audit_entries", insert_call.args[0])
        self.assertEqual(insert_call.args[1][2], "rollback")

    def test_holds_when_insufficient_samples(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(rows=[({"success": True},)]),
            fake_cursor(rows=[({"success": True},)]),
            fake_cursor(),
        ]
        result = evaluate_policy_candidate(conn, "t1", "v2", "v1")
        self.assertEqual(result["eval_status"], "insufficient_data")
        self.assertEqual(result["recommended_action"], "hold")


class RunEvaluationCycleTests(unittest.TestCase):
    def test_skips_tenants_with_single_policy_version(self):
        conn = MagicMock()
        conn.execute.side_effect = [
            fake_cursor(rows=[("t1",)]),  # list_tenant_ids
            fake_cursor(rows=[("v1",)]),  # list_policy_versions for t1 -> only 1
        ]
        results = run_evaluation_cycle(conn)
        self.assertEqual(results, [])


if __name__ == "__main__":
    unittest.main()
