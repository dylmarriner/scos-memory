"""Integration/regression tests for the real deterministic lexical benchmark gate."""
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[2]
FIXTURE = ROOT / "benchmarks/p4/retrieval_fixture.json"


def run_gate(*args):
    return subprocess.run(
        [sys.executable, "-m", "benchmarks.p4.offline_gate", *map(str, args)],
        cwd=ROOT, text=True, capture_output=True, check=False, timeout=30,
    )


class OfflineGateTests(unittest.TestCase):
    def test_ci_runs_when_shared_lexical_harness_changes(self):
        workflow = (ROOT / ".github/workflows/p4-hybrid-retrieval.yml").read_text()
        self.assertEqual(workflow.count('      - "benchmarks/harness.py"'), 2)

    def test_real_retrieval_record_then_check(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = pathlib.Path(directory) / "baseline.json"
            record = run_gate("--record-baseline", baseline)
            self.assertEqual(record.returncode, 0, record.stderr)
            data = json.loads(baseline.read_text())
            fixture = json.loads(FIXTURE.read_text())
            self.assertEqual(len(data["cases"]), len(fixture["cases"]))
            self.assertEqual(data["engine"], "deterministic_lexical_baseline")
            self.assertEqual(len(data["fixture_sha256"]), 64)
            self.assertTrue(any(case["retrieved_ids"] for case in data["cases"].values()))
            check = run_gate("--check", baseline)
            self.assertEqual(check.returncode, 0, check.stderr)
            self.assertIn("PASS", check.stdout)

    def test_rejects_regression_in_per_case_quality(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = pathlib.Path(directory) / "baseline.json"
            self.assertEqual(run_gate("--record-baseline", baseline).returncode, 0)
            data = json.loads(baseline.read_text())
            case = next(iter(data["cases"].values()))
            case["metrics"]["recall@5"] = 1.1
            baseline.write_text(json.dumps(data))
            result = run_gate("--check", baseline)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("recall@5", result.stderr)

    def test_rejects_missing_case_in_baseline(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = pathlib.Path(directory) / "baseline.json"
            self.assertEqual(run_gate("--record-baseline", baseline).returncode, 0)
            data = json.loads(baseline.read_text())
            data["cases"].pop(next(iter(data["cases"])))
            baseline.write_text(json.dumps(data))
            result = run_gate("--check", baseline)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("case", result.stderr.lower())

    def test_rejects_changed_fixture(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = pathlib.Path(directory) / "baseline.json"
            self.assertEqual(run_gate("--record-baseline", baseline).returncode, 0)
            data = json.loads(baseline.read_text())
            data["fixture_sha256"] = "0" * 64
            baseline.write_text(json.dumps(data))
            result = run_gate("--check", baseline)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("fixture", result.stderr.lower())

    def test_rejects_unknown_expected_corpus_id(self):
        with tempfile.TemporaryDirectory() as directory:
            altered = json.loads(FIXTURE.read_text())
            altered["cases"][0]["expected_ids"] = ["missing_document"]
            fixture = pathlib.Path(directory) / "fixture.json"
            fixture.write_text(json.dumps(altered))
            result = run_gate("--fixture", fixture, "--record-baseline", pathlib.Path(directory) / "out.json")
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("missing_document", result.stderr)

    def test_record_refuses_forbidden_exposure(self):
        # Baseline creation itself must fail closed on known-bad evidence.
        with tempfile.TemporaryDirectory() as directory:
            fixture = pathlib.Path(directory) / "contaminated.json"
            fixture.write_text(json.dumps({
                "name": "contaminated",
                "corpus": [
                    {"id": "forbidden", "text": "needle needle"},
                    {"id": "expected", "text": "needle"},
                ],
                "cases": [{
                    "id": "leak", "query": "needle",
                    "expected_ids": ["expected"], "forbidden_ids": ["forbidden"],
                }],
            }))
            baseline = pathlib.Path(directory) / "baseline.json"
            result = run_gate("--fixture", fixture, "--record-baseline", baseline)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn("forbidden", result.stderr.lower())
            self.assertFalse(baseline.exists())

    def test_rejects_contaminated_baseline_even_if_current_is_clean(self):
        # A recorded exposure must never become an accepted security budget.
        from benchmarks.p4.offline_gate import compare, load_fixture, run_retrieval
        fixture, digest = load_fixture(FIXTURE)
        current = run_retrieval(fixture, digest)
        baseline = json.loads(json.dumps(current))
        baseline["cases"]["memory_temporal"]["forbidden_at_5"] = 1
        self.assertTrue(any("forbidden" in issue for issue in compare(current, baseline)))

    def test_rejects_forbidden_exposure_even_when_rebaselined(self):
        # An unchanged bad baseline must not silently permit stale evidence.
        from benchmarks.p4.offline_gate import compare, load_fixture, run_retrieval
        fixture, digest = load_fixture(FIXTURE)
        current = run_retrieval(fixture, digest)
        current["cases"]["memory_temporal"]["forbidden_at_5"] = 1
        baseline = json.loads(json.dumps(current))
        self.assertTrue(any("forbidden" in issue for issue in compare(current, baseline)))

    def test_rejects_increased_forbidden_exposure(self):
        # Exercise the comparison directly with an injected degraded result;
        # the real lexical fixture currently has zero forbidden hits.
        from benchmarks.p4.offline_gate import compare, load_fixture, run_retrieval
        fixture, digest = load_fixture(FIXTURE)
        baseline = run_retrieval(fixture, digest)
        current = json.loads(json.dumps(baseline))
        case = next(iter(current["cases"].values()))
        case["forbidden_at_5"] += 1
        self.assertTrue(any("forbidden" in issue for issue in compare(current, baseline)))


    def _check_mutated_baseline(self, mutate):
        with tempfile.TemporaryDirectory() as directory:
            baseline = pathlib.Path(directory) / "baseline.json"
            recorded = run_gate("--record-baseline", baseline)
            self.assertEqual(recorded.returncode, 0, recorded.stderr)
            data = json.loads(baseline.read_text())
            mutate(data)
            baseline.write_text(json.dumps(data))
            result = run_gate("--check", baseline)
            self.assertNotEqual(result.returncode, 0, "corrupt baseline passed: " + result.stdout)
            return result

    def test_rejects_baseline_metric_lowering_without_rank_change(self):
        def mutate(data):
            data["cases"]["memory_exact"]["metrics"]["recall@5"] = 0.0
        result = self._check_mutated_baseline(mutate)
        self.assertIn("inconsistent", result.stderr)

    def test_rejects_baseline_ranked_ids_tampering(self):
        def mutate(data):
            case = data["cases"]["memory_exact"]
            case["retrieved_ids"][0] = "mem_live_deploy"
        result = self._check_mutated_baseline(mutate)
        self.assertIn("inconsistent", result.stderr)

    def test_rejects_baseline_out_of_corpus_ranked_id(self):
        def mutate(data):
            data["cases"]["memory_exact"]["retrieved_ids"] = ["invented_document"]
        result = self._check_mutated_baseline(mutate)
        self.assertIn("unknown", result.stderr)

    def test_rejects_baseline_aggregate_metric_tampering(self):
        def mutate(data):
            data["average_metrics"]["recall@5"] = 0.0
        result = self._check_mutated_baseline(mutate)
        self.assertIn("average_metrics", result.stderr)

    def test_rejects_baseline_forbidden_total_tampering(self):
        def mutate(data):
            data["total_forbidden_at_5"] = 99
        result = self._check_mutated_baseline(mutate)
        self.assertIn("total_forbidden_at_5", result.stderr)


if __name__ == "__main__":
    unittest.main()
