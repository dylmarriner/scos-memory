"""Tests for benchmarks/harness.py against GOVERNANCE.md §33.

§33 requires the versioned evaluation suite to cover ten named case
categories and to track Recall@K, Precision@K, MRR, nDCG, citation
accuracy, authority accuracy, conflict detection, duplicate rate,
stale-hit rate, promotion precision/recall, token efficiency and answer
regression. Each of these must come from what the system under test
returned; a signal the system does not report must be absent from the
summary, never a constant.
"""
import io
import json
import os
import pathlib
import sys
import tempfile
import threading
import unittest
from contextlib import redirect_stdout
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = pathlib.Path(__file__).parents[1]
sys.path.insert(0, str(ROOT))

from benchmarks.harness import (  # noqa: E402
    BenchmarkCase,
    BenchmarkResult,
    BenchmarkRunner,
    BenchmarkSuite,
    DeterministicLexicalBaseline,
    GatewayMemoryAdapter,
    RetrievalOutcome,
    answer_regression,
    authority_accuracy,
    citation_accuracy,
    compute_scores,
    compare_baseline,
    conflict_detection,
    create_default_frozen_suite,
    duplicate_rate,
    load_suite,
    main,
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

# Metrics every run measures from the retrieved ids alone.
RETRIEVAL_METRICS = {
    "recall@1", "precision@1", "mrr", "ndcg@5",
    "authority_accuracy", "conflict_detection", "duplicate_rate", "stale_hit_rate",
    "task_success",
}
# Metrics that need a signal a plain retriever does not report.
REPORTED_SIGNAL_METRICS = {"citation_accuracy", "promotion_precision", "promotion_recall", "token_efficiency"}


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

    def test_summary_reports_measured_metrics_and_omits_unreported_signals(self):
        suite = create_default_frozen_suite()
        runner = BenchmarkRunner(suite)
        runner.run(DeterministicLexicalBaseline(suite.corpus))
        summary = runner.summary()
        self.assertTrue(RETRIEVAL_METRICS.issubset(summary["average_scores"].keys()))
        self.assertTrue(REPORTED_SIGNAL_METRICS.isdisjoint(summary["average_scores"].keys()))
        self.assertNotIn("answer_regression", summary["average_scores"])
        self.assertEqual(set(summary["categories"]), REQUIRED_CATEGORIES)

    def test_reported_signals_are_scored_from_the_outcome(self):
        suite = create_default_frozen_suite()

        def retriever(query, context):
            ids = DeterministicLexicalBaseline(suite.corpus)(query, context)
            return RetrievalOutcome(ids=ids, cited_ids=ids[:1], promoted=bool(ids), tokens_used=400)

        runner = BenchmarkRunner(suite)
        runner.run(retriever)
        scores = runner.summary()["average_scores"]
        self.assertTrue(REPORTED_SIGNAL_METRICS.issubset(scores.keys()))
        # case_001 has a 200-token budget and 400 tokens were used.
        self.assertEqual(scores["token_efficiency"], 0.5)

    def test_answer_regression_compares_against_a_baseline_run(self):
        suite = create_default_frozen_suite()
        baseline = BenchmarkRunner(suite)
        baseline.run(DeterministicLexicalBaseline(suite.corpus))
        answered = sum(r.answer_correct for r in baseline.results)
        self.assertGreater(answered, 0)
        empty = BenchmarkRunner(suite)
        empty.run(lambda query, context: [])
        regression = empty.summary(baseline_results=baseline.results)["average_scores"]["answer_regression"]
        self.assertEqual(regression, answered / len(suite.cases))
        same = BenchmarkRunner(suite)
        same.run(DeterministicLexicalBaseline(suite.corpus))
        self.assertEqual(same.summary(baseline_results=baseline.results)["average_scores"]["answer_regression"], 0.0)

    def test_empty_retrieval_cannot_pass_conflict_detection_in_full_runner(self):
        suite = create_default_frozen_suite()
        contradiction = next(c for c in suite.cases if c.category == "contradiction")
        runner = BenchmarkRunner(BenchmarkSuite(name="conflict", version="1", cases=[contradiction]))
        result = runner.run(lambda query, context: ["unrelated"])[0]
        self.assertEqual(result.scores["conflict_detection"], 0.0)
        self.assertEqual(runner.summary()["average_scores"]["conflict_detection"], 0.0)

    def test_answer_correct_is_computed_from_the_retrieved_ids(self):
        suite = create_default_frozen_suite()
        runner = BenchmarkRunner(suite)
        result = runner.run_single(suite.cases[1], lambda query, context: ["sys_memory_item_9921a"])
        self.assertTrue(result.answer_correct)
        result = runner.run_single(suite.cases[1], lambda query, context: ["doc_generic_match"])
        self.assertFalse(result.answer_correct)


class SummaryBaselineIntegrityTests(unittest.TestCase):
    """A missing/duplicated case cannot improve reported answer regression."""

    def setUp(self):
        suite = create_default_frozen_suite()
        self.runner = BenchmarkRunner(suite)
        self.runner.run(DeterministicLexicalBaseline(suite.corpus))
        self.baseline = list(self.runner.results)

    def test_removed_current_case_fails_closed(self):
        self.runner.results = self.runner.results[:-1]
        with self.assertRaisesRegex(ValueError, "baseline.*case.*match"):
            self.runner.summary(baseline_results=self.baseline)

    def test_missing_baseline_case_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "baseline.*case.*match"):
            self.runner.summary(baseline_results=self.baseline[:-1])

    def test_duplicate_baseline_case_fails_closed(self):
        with self.assertRaisesRegex(ValueError, "duplicate.*case_id"):
            self.runner.summary(baseline_results=self.baseline + [self.baseline[0]])

    def test_duplicate_current_case_fails_closed(self):
        self.runner.results.append(self.runner.results[0])
        with self.assertRaisesRegex(ValueError, "duplicate.*case_id"):
            self.runner.summary(baseline_results=self.baseline)

    def test_removed_case_without_baseline_cannot_report_healthy_summary(self):
        self.runner.results = self.runner.results[:-1]
        with self.assertRaisesRegex(ValueError, "suite.*case.*match"):
            self.runner.summary()

    def test_foreign_case_without_baseline_cannot_replace_suite_case(self):
        from dataclasses import replace
        self.runner.results[0] = replace(self.runner.results[0], case_id="foreign_case")
        with self.assertRaisesRegex(ValueError, "suite.*case.*match"):
            self.runner.summary()

    def test_matching_forged_baseline_and_current_still_rejects_foreign_case(self):
        from dataclasses import replace
        self.runner.results[0] = replace(self.runner.results[0], case_id="foreign_case")
        baseline = [replace(self.baseline[0], case_id="foreign_case"), *self.baseline[1:]]
        with self.assertRaisesRegex(ValueError, "suite.*case.*match"):
            self.runner.summary(baseline_results=baseline)

    def test_programmatic_suite_duplicate_case_ids_cannot_be_summarized(self):
        from dataclasses import replace
        self.runner.suite.cases.append(replace(self.runner.suite.cases[0]))
        with self.assertRaisesRegex(ValueError, "duplicate.*suite.*case"):
            self.runner.summary()

    def test_foreign_case_fails_closed(self):
        from dataclasses import replace
        replaced = [replace(self.baseline[0], case_id="foreign_case"), *self.baseline[1:]]
        with self.assertRaisesRegex(ValueError, "baseline.*case.*match"):
            self.runner.summary(baseline_results=replaced)


class MetricFunctionTests(unittest.TestCase):
    def test_citation_accuracy_none_when_no_assertion_or_no_report(self):
        self.assertIsNone(citation_accuracy([], ["a"]))
        self.assertIsNone(citation_accuracy(["a"], None))

    def test_citation_accuracy_scores_correct_and_incorrect_citations(self):
        self.assertEqual(citation_accuracy(["a"], ["a"]), 1.0)
        self.assertEqual(citation_accuracy(["a"], ["b"]), 0.0)
        self.assertEqual(citation_accuracy(["a"], []), 0.0)

    def test_authority_accuracy_measures_top_k_authority_share(self):
        self.assertEqual(authority_accuracy(["a"], ["a", "b"], 2), 0.5)
        self.assertIsNone(authority_accuracy([], ["a"], 2))

    def test_conflict_detection_flags_both_sides_surfaced(self):
        self.assertEqual(conflict_detection(["a", "b"], ["a", "b"]), 0.0)
        self.assertEqual(conflict_detection(["a", "b"], ["a"]), 1.0)

    def test_conflict_detection_does_not_reward_missing_evidence(self):
        # A retriever that returns no side of a conflict has not resolved it.
        self.assertEqual(conflict_detection(["a", "b"], []), 0.0)
        self.assertEqual(conflict_detection(["a", "b"], ["unrelated"]), 0.0)
        self.assertEqual(conflict_detection(["a", "b"], ["a", "a"]), 1.0)
        self.assertIsNone(conflict_detection(["a"], ["a"]))

    def test_duplicate_and_stale_rates(self):
        self.assertEqual(duplicate_rate(["dup"], ["dup", "real"]), 0.5)
        self.assertEqual(stale_hit_rate(["old"], ["old", "real"]), 0.5)
        self.assertIsNone(duplicate_rate([], ["real"]))
        self.assertIsNone(stale_hit_rate([], ["real"]))

    def test_token_efficiency_capped_at_one(self):
        self.assertEqual(token_efficiency(100, 50), 1.0)
        self.assertEqual(token_efficiency(100, 200), 0.5)
        self.assertIsNone(token_efficiency(0, 50))
        self.assertIsNone(token_efficiency(100, None))

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

    def test_promotion_is_absent_when_no_decision_was_reported(self):
        cases = [BenchmarkCase(id="c1", query="q", expected_ids=[], category="known_answer", promotion_expected=True)]
        results = [BenchmarkResult(case_id="c1", retrieved_ids=[], latency_ms=0)]
        self.assertEqual(promotion_precision_recall(cases, results), {})

    def test_compute_scores_without_case_or_result_is_base_metrics_only(self):
        scores = compute_scores({"a"}, ["a"])
        self.assertNotIn("citation_accuracy", scores)
        self.assertIn("recall@1", scores)


class BenchmarkComparisonSafetyTests(unittest.TestCase):
    def item(self, case, scores):
        return BenchmarkResult(case_id=case, retrieved_ids=["a"], latency_ms=0, scores=scores)

    def test_lower_stale_and_duplicate_rates_are_improvements(self):
        baseline = [self.item("case", {"stale_hit_rate": 0.75, "duplicate_rate": 0.5,
                                       "conflict_detection": 0.25, "recall@5": 0.6})]
        changed = [self.item("case", {"stale_hit_rate": 0.25, "duplicate_rate": 0.0,
                                      "conflict_detection": 0.75, "recall@5": 0.7})]
        verdict = compare_baseline(changed, baseline)
        for metric in ("stale_hit_rate", "duplicate_rate", "conflict_detection", "recall@5"):
            self.assertEqual(verdict[metric]["improvements"], 1, metric)
            self.assertEqual(verdict[metric]["regressions"], 0, metric)

    def test_increased_stale_and_duplicate_rates_are_regressions(self):
        baseline = [self.item("case", {"stale_hit_rate": 0.0, "duplicate_rate": 0.1})]
        changed = [self.item("case", {"stale_hit_rate": 1.0, "duplicate_rate": 0.9})]
        verdict = compare_baseline(changed, baseline)
        for metric in ("stale_hit_rate", "duplicate_rate"):
            self.assertEqual(verdict[metric]["regressions"], 1, metric)
            self.assertEqual(verdict[metric]["improvements"], 0, metric)

    def test_missing_optional_metric_is_reported_as_coverage_loss(self):
        baseline = [self.item("case", {"recall@5": 0.75, "citation_accuracy": 0.9})]
        changed = [self.item("case", {"recall@5": 0.75})]
        verdict = compare_baseline(changed, baseline)
        self.assertEqual(verdict["citation_accuracy"]["coverage_lost"], 1)
        self.assertEqual(verdict["citation_accuracy"]["regressions"], 0)
        self.assertEqual(verdict["recall@5"]["unchanged"], 1)

    def test_removed_case_is_not_silently_excluded_from_comparison(self):
        baseline = [self.item("retained", {"recall@5": 0.5}),
                    self.item("removed", {"recall@5": 0.0, "citation_accuracy": 0.0})]
        changed = [self.item("retained", {"recall@5": 0.6})]
        verdict = compare_baseline(changed, baseline)
        self.assertEqual(verdict["recall@5"]["improvements"], 1)
        self.assertEqual(verdict["recall@5"]["coverage_lost"], 1)
        self.assertEqual(verdict["citation_accuracy"]["coverage_lost"], 1)

    def test_added_case_is_reported_as_coverage_gain(self):
        baseline = [self.item("retained", {"recall@5": 0.5})]
        changed = [self.item("retained", {"recall@5": 0.5}),
                   self.item("added", {"recall@5": 1.0, "citation_accuracy": 1.0})]
        verdict = compare_baseline(changed, baseline)
        self.assertEqual(verdict["recall@5"]["unchanged"], 1)
        self.assertEqual(verdict["recall@5"]["coverage_gained"], 1)
        self.assertEqual(verdict["citation_accuracy"]["coverage_gained"], 1)

    def test_duplicate_case_ids_fail_closed_in_either_run(self):
        baseline = [self.item("same", {"recall@5": 0.1})]
        changed = [self.item("same", {"recall@5": 0.8}),
                   self.item("same", {"recall@5": 0.9})]
        with self.assertRaisesRegex(ValueError, "duplicate.*case_id"):
            compare_baseline(changed, baseline)
        with self.assertRaisesRegex(ValueError, "duplicate.*case_id"):
            compare_baseline(baseline, changed)

    def test_newly_measured_optional_metric_is_coverage_gain(self):
        baseline = [self.item("case", {"recall@5": 0.75})]
        changed = [self.item("case", {"recall@5": 0.75, "citation_accuracy": 0.9})]
        verdict = compare_baseline(changed, baseline)
        self.assertEqual(verdict["citation_accuracy"]["coverage_gained"], 1)
        self.assertEqual(verdict["citation_accuracy"]["improvements"], 0)


class SuiteFileTests(unittest.TestCase):
    def _write(self, payload):
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
        json.dump(payload, handle)
        handle.close()
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def test_load_suite_and_run_it_from_the_command_line(self):
        path = self._write({
            "name": "tiny", "version": "1", "corpus": {"a": "alpha beta", "b": "gamma delta"},
            "cases": [{"id": "t1", "query": "alpha", "expected_ids": ["a"], "category": "known_answer"}],
        })
        suite = load_suite(path)
        self.assertEqual([case.id for case in suite.cases], ["t1"])
        output = io.StringIO()
        with redirect_stdout(output):
            self.assertEqual(main(["--suite", path]), 0)
        report = json.loads(output.getvalue())
        self.assertEqual(report["baseline"]["summary"]["suite"], "tiny")
        self.assertEqual(report["baseline"]["summary"]["average_scores"]["recall@1"], 1.0)
        self.assertEqual(report["system_under_test"]["status"], "not_run")

    def test_load_suite_rejects_duplicate_case_ids(self):
        case = {"id": "same", "query": "alpha", "expected_ids": ["a"], "category": "known_answer"}
        path = self._write({"name": "bad", "version": "1", "corpus": {"a": "alpha"},
                            "cases": [case, dict(case, query="different")]})
        with self.assertRaisesRegex(ValueError, "duplicate.*case.*id"):
            load_suite(path)

    def test_load_suite_rejects_duplicate_json_keys_at_any_depth(self):
        for raw in (
            '{"name":"a","name":"b","version":"1","corpus":{"a":"alpha"},"cases":[]}',
            '{"name":"a","version":"1","corpus":{"a":"alpha","a":"other"},"cases":[]}',
            '{"name":"a","version":"1","corpus":{"a":"alpha"},"cases":[{"id":"c","id":"other","query":"alpha","expected_ids":["a"],"category":"known_answer"}]}',
        ):
            with self.subTest(raw=raw):
                handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
                handle.write(raw)
                handle.close()
                self.addCleanup(os.unlink, handle.name)
                with self.assertRaisesRegex(ValueError, "duplicate.*JSON key"):
                    load_suite(handle.name)

    def test_load_suite_rejects_nonfinite_json_numbers(self):
        for constant in ("NaN", "Infinity", "-Infinity", "1e999"):
            with self.subTest(constant=constant):
                handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False)
                handle.write('{"name":"a","version":"1","corpus":{"a":"alpha"},"cases":[{"id":"c","query":"alpha","expected_ids":["a"],"category":"known_answer","token_budget":'+constant+'}]}')
                handle.close()
                self.addCleanup(os.unlink, handle.name)
                with self.assertRaisesRegex(ValueError, "non-finite.*JSON"):
                    load_suite(handle.name)

    def test_load_suite_rejects_ids_outside_the_corpus(self):
        path = self._write({
            "name": "bad", "version": "1", "corpus": {"a": "alpha"},
            "cases": [{"id": "t1", "query": "alpha", "expected_ids": ["missing"], "category": "known_answer"}],
        })
        with self.assertRaisesRegex(ValueError, "not in the corpus"):
            load_suite(path)


class _Gateway(BaseHTTPRequestHandler):
    """An MCP endpoint answering scos_remember/scos_recall/scos_forget over an
    in-memory store, with one memory that predates the benchmark run."""
    facts: dict = {}
    forgotten: list = []
    fail_forget: bool = False
    refuse_forget_ids: set = set()
    forget_requests: list = []
    tool_errors: set = set()

    def do_POST(self):  # noqa: N802
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        name, args = request["params"]["name"], request["params"]["arguments"]
        if name in self.tool_errors:
            # MCP tool-level failure can still be HTTP 200 and contain a
            # well-formed JSON body that resembles a successful tool result.
            payload = ({"forgotten": True} if name == "scos_forget" else
                       {"recall": [{"memory_id": "fact-2"}]} if name == "scos_recall" else
                       {"fact": {"id": "fact-2", "content": args["content"]}})
            body = json.dumps({"jsonrpc": "2.0", "id": request["id"],
                               "result": {"isError": True, "content": [
                                   {"type": "text", "text": json.dumps(payload)}]}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        if name == "scos_remember":
            content = args["content"]
            existing = next((fid for fid, text in self.facts.items() if text.split(" [")[0] == content.split(" [")[0]), None)
            if existing is None:
                existing = f"fact-{len(self.facts) + 1}"
                self.facts[existing] = content
            payload = {"fact": {"id": existing, "content": self.facts[existing]}}
        elif name == "scos_recall":
            words = set(args["query"].lower().split())
            ranked = sorted(((len(words & set(text.lower().split())), fid) for fid, text in self.facts.items()), reverse=True)
            payload = {"recall": [{"memory_id": fid} for score, fid in ranked if score][: args["max_items"]]}
        elif name == "scos_forget":
            self.forget_requests.append(args["memory_id"])
            if self.fail_forget or args["memory_id"] in self.refuse_forget_ids:
                payload = {"forgotten": False}
            else:
                self.forgotten.append(args["memory_id"])
                self.facts.pop(args["memory_id"], None)
                payload = {"forgotten": True}
        else:
            raise AssertionError(f"unexpected MCP tool: {name}")
        body = json.dumps({"jsonrpc": "2.0", "id": request["id"],
                           "result": {"content": [{"type": "text", "text": json.dumps(payload)}]}}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        return


class GatewayMemoryAdapterTests(unittest.TestCase):
    def setUp(self):
        _Gateway.facts = {"user-1": "alpha beta preexisting"}
        _Gateway.forgotten = []
        _Gateway.fail_forget = False
        _Gateway.refuse_forget_ids = set()
        _Gateway.forget_requests = []
        _Gateway.tool_errors = set()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), _Gateway)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.url = f"http://127.0.0.1:{self.server.server_port}/mcp"

    def test_mcp_tool_error_cannot_fake_successful_seeding(self):
        _Gateway.tool_errors = {"scos_remember"}
        with self.assertRaisesRegex(RuntimeError, "scos_remember.*failed"):
            with GatewayMemoryAdapter(self.url, {"b": "gamma delta"}):
                pass
        self.assertNotIn("fact-2", _Gateway.facts)

    def test_mcp_tool_error_cannot_fake_successful_recall(self):
        with GatewayMemoryAdapter(self.url, {"b": "gamma delta"}) as adapter:
            _Gateway.tool_errors = {"scos_recall"}
            with self.assertRaisesRegex(RuntimeError, "scos_recall.*failed"):
                adapter("gamma delta", {})
            _Gateway.tool_errors.clear()
        self.assertEqual(_Gateway.forgotten, ["fact-2"])

    def test_mcp_tool_error_cannot_fake_confirmed_cleanup(self):
        adapter = GatewayMemoryAdapter(self.url, {"b": "gamma delta"})
        with self.assertRaisesRegex(RuntimeError, "cleanup"):
            with adapter:
                _Gateway.tool_errors = {"scos_forget"}
        self.assertIn("fact-2", _Gateway.facts)
        self.assertTrue(any("fact-2" in item for item in adapter.forget_failures))
        self.assertEqual(_Gateway.forgotten, [])

    def test_seeds_maps_recall_and_forgets_only_what_it_created(self):
        corpus = {"a": "alpha beta preexisting", "b": "gamma delta", "c": "epsilon zeta"}
        suite = BenchmarkSuite(name="s", version="1", corpus=corpus, cases=[
            BenchmarkCase(id="t1", query="gamma delta", expected_ids=["b"], category="known_answer"),
        ])
        with GatewayMemoryAdapter(self.url, corpus) as adapter:
            runner = BenchmarkRunner(suite)
            runner.run(adapter)
            recalled = adapter("alpha", {})
            report = adapter.seed_report()
        self.assertEqual(runner.results[0].retrieved_ids[0], "b")
        self.assertTrue(runner.results[0].answer_correct)
        # "a" merged into the user's existing memory: recalled under its corpus id, never forgotten.
        self.assertEqual(recalled, ["a"])
        self.assertIn("a", report["merged_on_save"])
        self.assertEqual(sorted(_Gateway.forgotten), ["fact-2", "fact-3"])
        self.assertIn("user-1", _Gateway.facts)

    def test_cleanup_refusal_is_reported_and_fails_closed(self):
        _Gateway.fail_forget = True
        adapter = GatewayMemoryAdapter(self.url, {"b": "gamma delta"})
        with self.assertRaisesRegex(RuntimeError, "cleanup"):
            with adapter:
                self.assertEqual(adapter("gamma", {}), ["b"])
        self.assertIn("fact-2", _Gateway.facts)
        self.assertEqual(_Gateway.forgotten, [])
        self.assertTrue(any("fact-2" in item for item in adapter.seed_report()["forget_failures"]))

    def test_cleanup_continues_after_first_refusal(self):
        # LIFO cleanup first attempts fact-3, then must still forget fact-2.
        _Gateway.refuse_forget_ids = {"fact-3"}
        adapter = GatewayMemoryAdapter(self.url, {"b": "gamma delta", "c": "epsilon zeta"})
        with self.assertRaisesRegex(RuntimeError, "cleanup"):
            with adapter:
                pass
        self.assertEqual(_Gateway.forget_requests, ["fact-3", "fact-2"])
        self.assertEqual(_Gateway.forgotten, ["fact-2"])
        self.assertIn("fact-3", _Gateway.facts)
        self.assertNotIn("fact-2", _Gateway.facts)
        self.assertTrue(any("fact-3" in item for item in adapter.seed_report()["forget_failures"]))

    def test_cleanup_refusal_does_not_mask_evaluation_failure(self):
        _Gateway.fail_forget = True
        adapter = GatewayMemoryAdapter(self.url, {"b": "gamma delta"})
        with self.assertRaisesRegex(ValueError, "evaluation failed") as caught:
            with adapter:
                raise ValueError("evaluation failed")
        self.assertIn("fact-2", "\n".join(getattr(caught.exception, "__notes__", [])))
        self.assertTrue(adapter.seed_report()["forget_failures"])
        self.assertIn("fact-2", _Gateway.facts)

    def test_partial_seeding_failure_reports_residual_memory_ids(self):
        # Seed the first memory over HTTP, refuse its HTTP cleanup, then fail
        # the next seed. The primary seeding error must retain cleanup evidence.
        _Gateway.refuse_forget_ids = {"fact-2"}
        adapter = GatewayMemoryAdapter(self.url, {"b": "gamma delta", "c": "epsilon zeta"})
        original_call = adapter._call

        def fail_second_seed(tool, arguments):
            if tool == "scos_remember" and arguments["content"].startswith("epsilon zeta"):
                raise RuntimeError("second seed rejected")
            return original_call(tool, arguments)

        adapter._call = fail_second_seed
        with self.assertRaisesRegex(RuntimeError, "second seed rejected") as caught:
            with adapter:
                pass
        self.assertEqual(_Gateway.forget_requests, ["fact-2"])
        self.assertIn("fact-2", _Gateway.facts)
        self.assertIn("fact-2", "\n".join(getattr(caught.exception, "__notes__", [])))
        self.assertTrue(adapter.seed_report()["forget_failures"])

    def test_main_does_not_emit_success_report_before_cleanup(self):
        _Gateway.fail_forget = True
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        path = pathlib.Path(temp.name) / "suite.json"
        path.write_text(json.dumps({
            "name": "cleanup", "version": "1",
            "corpus": {"b": "gamma delta"},
            "cases": [{"id": "t1", "query": "gamma", "category": "known_answer",
                       "expected_ids": ["b"]}],
        }), encoding="utf-8")
        output = io.StringIO()
        with self.assertRaisesRegex(RuntimeError, "cleanup"):
            with redirect_stdout(output):
                main(["--suite", str(path), "--endpoint", self.url])
        self.assertEqual(output.getvalue(), "")
        self.assertIn("fact-2", _Gateway.facts)

    def test_main_success_report_requires_completed_cleanup(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        path = pathlib.Path(temp.name) / "suite.json"
        path.write_text(json.dumps({
            "name": "cleanup", "version": "1",
            "corpus": {"b": "gamma delta"},
            "cases": [{"id": "t1", "query": "gamma", "category": "known_answer",
                       "expected_ids": ["b"]}],
        }), encoding="utf-8")
        output = io.StringIO()
        with redirect_stdout(output):
            self.assertEqual(main(["--suite", str(path), "--endpoint", self.url]), 0)
        report = json.loads(output.getvalue())
        self.assertEqual(report["seeding"]["forget_failures"], [])
        self.assertEqual(_Gateway.forgotten, ["fact-2"])
        self.assertNotIn("fact-2", _Gateway.facts)

    def test_unavailable_gateway_fails_closed(self):
        with self.assertRaisesRegex(RuntimeError, "unavailable"):
            with GatewayMemoryAdapter("http://127.0.0.1:1/mcp", {"a": "alpha"}):
                pass


if __name__ == "__main__":
    unittest.main()
