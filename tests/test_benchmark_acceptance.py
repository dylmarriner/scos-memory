import json
import pathlib
import unittest
import sys


ROOT = pathlib.Path(__file__).parents[1]
ARTIFACT = ROOT / "benchmarks/acceptance-results.json"
sys.path.insert(0, str(ROOT))

from benchmarks.run_acceptance import build_artifact, frozen_acceptance_cases  # noqa: E402


class BenchmarkAcceptanceEvidenceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.payload = json.loads(ARTIFACT.read_text(encoding="utf-8"))

    def test_schema_and_comparative_evidence(self):
        payload = self.payload
        self.assertEqual(set(payload["suite"]["domains"]), {
            "context_optimization", "code_intelligence", "execution_proxy", "control_plane_routing",
        })
        self.assertGreaterEqual(len(payload["algorithms"]), 8)
        names = {algorithm["name"] for algorithm in payload["algorithms"]}
        self.assertIn("context_optimised_local", names)
        self.assertIn("unoptimised_context_baseline", names)
        self.assertEqual(len(payload["comparisons"]), 4)
        self.assertTrue(all("thresholds" in comparison for comparison in payload["comparisons"]))
        self.assertEqual(set(payload["task_success"]), names)
        for algorithm in payload["algorithms"]:
            self.assertGreater(len(algorithm["cases"]), 0)
            self.assertTrue(algorithm["confidence_intervals"])
            self.assertIn("total_cost_usd", algorithm["summary"])
            self.assertTrue(all("latency_ms" in case for case in algorithm["cases"]))

    def test_evidence_is_non_empty_and_measured(self):
        for algorithm in self.payload["algorithms"]:
            self.assertTrue(any(case["measured"] for case in algorithm["cases"]))
            self.assertTrue(all(case["latency_ms"] >= 0 for case in algorithm["cases"]))
            self.assertTrue(all(case["cost_usd"] >= 0 for case in algorithm["cases"]))
        self.assertTrue(self.payload["hardware"]["python"])

    def test_frozen_cases_are_explicit_and_locally_measured(self):
        cases = frozen_acceptance_cases()
        self.assertEqual(sum(len(domain_cases) for domain_cases in cases.values()), 87)
        self.assertEqual(self.payload["suite"]["frozen_created_at"], "2026-07-27T00:00:00Z")
        self.assertTrue(all(algorithm["metadata"]["runner"] == "measured_local"
                            for algorithm in self.payload["algorithms"]))

    def test_live_mode_fails_closed_when_service_is_unavailable(self):
        with self.assertRaises(RuntimeError):
            build_artifact("http://127.0.0.1:1", include_live=True)


if __name__ == "__main__":
    unittest.main()
