import json
import pathlib
import sys
import unittest
from unittest.mock import patch

ROOT = pathlib.Path(__file__).parents[1]
ARTIFACT = ROOT / "benchmarks/acceptance-results.json"
sys.path.insert(0, str(ROOT))

from benchmarks import run_acceptance  # noqa: E402
from benchmarks.local_services import StackUnavailable  # noqa: E402
from benchmarks.run_acceptance import (  # noqa: E402
    build_artifact,
    code_metrics,
    context_metrics,
    count_tokens,
    frozen_acceptance_cases,
    proxy_metrics,
    run_domain,
)

DOMAINS = {"context_optimization", "code_intelligence", "execution_proxy", "control_plane_routing"}


def _contains_key(value, key):
    if isinstance(value, dict):
        return key in value or any(_contains_key(v, key) for v in value.values())
    if isinstance(value, list):
        return any(_contains_key(v, key) for v in value)
    return False


class CommittedArtifactTests(unittest.TestCase):
    """The committed artifact is the output of an actual run."""

    @classmethod
    def setUpClass(cls):
        cls.payload = json.loads(ARTIFACT.read_text(encoding="utf-8"))

    def test_every_domain_is_measured_or_says_why_not(self):
        payload = self.payload
        self.assertEqual(set(payload["suite"]["domains"]), DOMAINS)
        self.assertEqual(set(payload["domain_status"]), DOMAINS)
        self.assertEqual({c["domain"] for c in payload["comparisons"]}, DOMAINS)
        for comparison in payload["comparisons"]:
            status = payload["domain_status"][comparison["domain"]]["status"]
            self.assertEqual(comparison["status"], status)
            if status == "not_run":
                self.assertTrue(comparison["reason"])
                self.assertFalse(comparison["all_thresholds_pass"])
                self.assertFalse(any(a["domain"] == comparison["domain"] for a in payload["algorithms"]))
            else:
                self.assertEqual(set(comparison["thresholds"]), set(payload["thresholds"][comparison["domain"]]))

    def test_measured_algorithms_carry_per_case_evidence(self):
        names = {algorithm["name"] for algorithm in self.payload["algorithms"]}
        self.assertEqual(set(self.payload["task_success"]), names)
        for algorithm in self.payload["algorithms"]:
            self.assertIn(algorithm["metadata"]["runner"], {"measured_service", "measured_baseline"})
            self.assertTrue(algorithm["metadata"]["system_under_test"])
            self.assertGreater(len(algorithm["cases"]), 0)
            self.assertTrue(algorithm["confidence_intervals"])
            self.assertTrue(all(case["latency_ms"] >= 0 for case in algorithm["cases"]))
            self.assertTrue(all(case["measured"] for case in algorithm["cases"]))

    def test_no_invented_cost_and_a_named_tokenizer(self):
        for key in ("cost_usd", "total_cost_usd", "costs", "cost_per_successful_task"):
            self.assertFalse(_contains_key(self.payload, key), key)
        self.assertEqual(self.payload["tokenizer"]["name"], run_acceptance.TOKENIZER["name"])

    def test_artifact_records_its_source_and_live_status(self):
        self.assertTrue(self.payload["source_sha"])
        self.assertTrue(self.payload["hardware"]["python"])
        if "live" not in self.payload:
            self.assertTrue(any("live service evidence not included" in e for e in self.payload["exclusions"]))


class FrozenCaseTests(unittest.TestCase):
    def test_frozen_cases_are_explicit(self):
        cases = frozen_acceptance_cases()
        self.assertEqual(sum(len(domain_cases) for domain_cases in cases.values()), 87)
        for case in cases["context_optimization"]:
            self.assertIn(case["expected"]["instruction"], case["input"])
        for case in cases["execution_proxy"]:
            self.assertIn("failure", case)
            if not case["failure"]:
                self.assertIn("deterministic", case)


class MetricTests(unittest.TestCase):
    """Metrics are computed from what the system returned, against ground
    truth it never received."""

    def setUp(self):
        self.cases = frozen_acceptance_cases()

    def test_context_metrics_fail_an_output_that_drops_the_exact_value(self):
        case = self.cases["context_optimization"][3]  # op-7734
        dropped = case["expected"]["instruction"].replace("op-7734", "")
        metrics = context_metrics(case, {"output": dropped, "restored": case["input"]})
        self.assertEqual(metrics["exact_value_retention"], 0.0)
        self.assertEqual(metrics["mandatory_instruction_retention"], 0.0)
        self.assertEqual(metrics["task_success"], 0.0)
        self.assertGreater(metrics["total_token_reduction"], 0.75)

    def test_context_restoration_compares_bytes(self):
        case = self.cases["context_optimization"][0]
        output = case["expected"]["instruction"]
        self.assertEqual(context_metrics(case, {"output": output, "restored": case["input"]})["restoration_exact"], 1.0)
        self.assertEqual(context_metrics(case, {"output": output, "restored": case["input"].strip()})["restoration_exact"], 0.0)
        self.assertEqual(context_metrics(case, {"output": output, "restored": None})["restoration_exact"], 0.0)

    def test_code_metrics_use_the_returned_files(self):
        case = self.cases["code_intelligence"][1]  # two expected files
        partial = code_metrics(case, {"matches": [case["expected"][0]]})
        self.assertEqual(partial["exact_lookup"], 0.0)
        self.assertEqual(partial["symbol_definition_recall"], 0.5)
        self.assertEqual(partial["reference_precision"], 1.0)
        negative = next(c for c in self.cases["code_intelligence"] if not c["expected"])
        self.assertEqual(code_metrics(negative, {"matches": []})["exact_lookup"], 1.0)
        self.assertEqual(code_metrics(negative, {"matches": ["x.go"]})["reference_precision"], 0.0)

    def _run(self, stdout, stderr=b"", status=0):
        return {"stdout": stdout, "stderr": stderr, "exit_status": status}

    def test_proxy_restoration_compares_restored_bytes_with_the_command_output(self):
        case = {"id": "p", "command": "printf x", "failure": False, "deterministic": True}
        reference = self._run(b"line one\nline two\n")
        observed = {"executed": True, "blocked": False, "exit_status": 0, "delivered": "line one",
                    "reference": reference, "second_reference": dict(reference),
                    "restored": {"stdout": b"line one\n", "stderr": b""}}
        metrics = proxy_metrics(case, observed)
        self.assertEqual(metrics["raw_result_restoration"], 0.0)
        self.assertEqual(metrics["task_success"], 0.0)
        self.assertNotIn("restored", observed)  # raw bytes replaced by their measurements

    def test_proxy_token_reduction_counts_output_tokens(self):
        case = {"id": "p", "command": "printf x", "failure": False, "deterministic": True}
        raw = b"diagnostic\n" * 100
        reference = self._run(raw)
        observed = {"executed": True, "blocked": False, "exit_status": 0, "delivered": "100 lines: diagnostic",
                    "reference": reference, "second_reference": dict(reference),
                    "restored": {"stdout": raw, "stderr": b""}}
        metrics = proxy_metrics(case, observed)
        self.assertEqual(metrics["raw_result_restoration"], 1.0)
        self.assertAlmostEqual(metrics["token_reduction"], 1 - count_tokens("100 lines: diagnostic") / 100)

    def test_proxy_failure_preservation_needs_exit_status_and_diagnostics(self):
        case = {"id": "p", "command": "cat missing", "failure": False, "deterministic": True}
        reference = self._run(b"", b"cat: missing: No such file or directory\n", 1)
        observed = {"executed": True, "blocked": False, "exit_status": 1, "delivered": "1 failure",
                    "reference": reference, "second_reference": dict(reference),
                    "restored": {"stdout": b"", "stderr": reference["stderr"]}}
        self.assertEqual(proxy_metrics(case, observed)["failure_preservation"], 0.0)

    def test_proxy_live_state_commands_are_excluded_from_byte_restoration(self):
        case = {"id": "p", "command": "find / -name x", "failure": False, "deterministic": False}
        reference = self._run(b"a\n", b"find: '/proc/101': No such file or directory\n", 1)
        observed = {"executed": True, "blocked": False, "exit_status": 1,
                    "delivered": "b\nfind: '/proc/202': No such file or directory",
                    "reference": reference, "second_reference": dict(reference),
                    "restored": {"stdout": b"b\n", "stderr": b"find: '/proc/202': No such file or directory\n"}}
        metrics = proxy_metrics(case, observed)
        self.assertNotIn("raw_result_restoration", metrics)
        self.assertIn("restoration_excluded", observed)
        # Diagnostics are judged against that execution's own stderr.
        self.assertEqual(metrics["failure_preservation"], 1.0)

    def test_destructive_commands_score_only_on_being_blocked(self):
        case = {"id": "p", "command": "rm -rf /", "failure": True}
        self.assertEqual(proxy_metrics(case, {"executed": False, "blocked": True}),
                         {"destructive_blocked": 1.0, "task_success": 1.0})
        self.assertEqual(proxy_metrics(case, {"executed": False, "blocked": False})["destructive_blocked"], 0.0)


class NotRunTests(unittest.TestCase):
    def test_a_domain_whose_service_cannot_start_is_not_run_and_never_scored(self):
        class Unavailable:
            def __init__(self, services):
                raise StackUnavailable("docker is not installed")

        with patch.object(run_acceptance, "LocalServiceStack", Unavailable):
            algorithms, evaluation = run_domain("code_intelligence", frozen_acceptance_cases()["code_intelligence"])
        self.assertEqual(algorithms, [])
        self.assertEqual(evaluation["status"], "not_run")
        self.assertIn("docker", evaluation["reason"])
        self.assertFalse(evaluation["all_thresholds_pass"])

    def test_live_mode_fails_closed_when_the_gateway_is_unavailable(self):
        with self.assertRaises(RuntimeError):
            build_artifact("http://127.0.0.1:1/mcp", include_live=True)
        with self.assertRaises(RuntimeError):
            build_artifact(None, include_live=True)


if __name__ == "__main__":
    unittest.main()


class DestructiveGuardMeasurementTests(unittest.TestCase):
    """The proxy's destructive guard is measured on /v1/execute with a
    command confined to a sentinel directory; the case's own command is
    only ever planned."""

    def _service(self, workspace):
        service = object.__new__(run_acceptance.ProxyService)
        service.url, service.headers = "https://proxy", {}
        service.identity = {"tenant_id": "t", "principal_id": "p"}
        service.workspace = workspace
        return service

    def _measure(self, plan_risk, execute):
        import tempfile
        calls = []

        def fake(method, url, headers, payload=None, **_):
            calls.append((url, payload["command"]))
            if url.endswith("/v1/plan"):
                return {"risk_level": plan_risk}
            return execute(payload["command"])

        with tempfile.TemporaryDirectory() as tmp, patch.object(run_acceptance, "request_json", fake):
            observed = self._service(pathlib.Path(tmp))({"id": "proxy_002", "command": "rm -rf /", "failure": True})
            leftovers = list(pathlib.Path(tmp).iterdir())
        self.assertEqual(calls[0], ("https://proxy/v1/plan", "rm -rf /"))
        self.assertNotIn(("https://proxy/v1/execute", "rm -rf /"), calls)
        self.assertEqual(leftovers, [])
        return observed

    def test_refused_probe_with_destructive_plan_is_blocked(self):
        def refuse(_command):
            raise run_acceptance.HTTPError(403, "destructive command refused: tool governance has no recorded confirmation")
        observed = self._measure("destructive", refuse)
        self.assertTrue(observed["blocked"])
        self.assertTrue(observed["sentinel_survived"])
        self.assertEqual(proxy_metrics({"failure": True}, observed)["destructive_blocked"], 1.0)

    def test_probe_that_runs_is_not_blocked(self):
        import shutil

        def run(command):
            shutil.rmtree(command.removeprefix("rm -rf "))
            return {"exit_status": 0}
        observed = self._measure("destructive", run)
        self.assertFalse(observed["blocked"])
        self.assertTrue(observed["executed"])

    def test_plan_that_misgrades_the_case_is_not_blocked(self):
        def refuse(_command):
            raise run_acceptance.HTTPError(403, "no recorded confirmation")
        self.assertFalse(self._measure("write", refuse)["blocked"])
