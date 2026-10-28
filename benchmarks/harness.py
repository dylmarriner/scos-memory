#!/usr/bin/env python3
"""
Benchmark harness (§33 Quality evaluation).

Measures Recall@K, Precision@K, MRR, nDCG, authority accuracy, conflict
detection, duplicate rate, stale-hit rate and answer regression from what a
retrieval system actually returns. Citation accuracy, promotion
precision/recall and token efficiency are computed only when the system
under test reports citations, promotion decisions or token usage; a run that
observes none of them reports none of them, rather than a constant.

Usage::

    python3 benchmarks/harness.py --endpoint https://127.0.0.1:8098/mcp --ca-file ca.crt
    python3 benchmarks/harness.py --suite path/to/suite.json --endpoint ...
    python3 benchmarks/harness.py            # lexical baseline only

With ``--endpoint`` the suite corpus is seeded into a deployed SCOS through
its MCP gateway (``scos_remember``), every case is answered by
``scos_recall``, and the seeded memories are removed (``scos_forget``)
afterwards.
"""
from __future__ import annotations

import argparse
import json
import math
import platform
import random
import ssl
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable

# ── benchmark types ──────────────────────────────────────────────────────

@dataclass
class BenchmarkCase:
    id: str
    query: str
    expected_ids: list[str]
    # known_answer, exact_identifier, temporal, contradiction, permissions,
    # project_isolation, multi_turn, recovery, stale, prompt_injection
    category: str
    context: dict[str, Any] = field(default_factory=dict)
    # Ground-truth semantics beyond plain relevance: which ids must be
    # cited, which are authoritative, which pair off as an active/stale
    # contradiction, which are known duplicates or known-stale, whether this
    # case's memory should be promoted, and an approximate token budget.
    citation_ids: list[str] = field(default_factory=list)
    authoritative_ids: list[str] = field(default_factory=list)
    conflicting_ids: list[str] = field(default_factory=list)
    duplicate_ids: list[str] = field(default_factory=list)
    stale_ids: list[str] = field(default_factory=list)
    promotion_expected: bool = False
    token_budget: int = 0


@dataclass
class RetrievalOutcome:
    """What a system under test returned for one case. Fields other than
    ``ids`` are None when the system does not report that signal."""
    ids: list[str]
    cited_ids: list[str] | None = None
    promoted: bool | None = None
    tokens_used: int | None = None


@dataclass
class BenchmarkResult:
    case_id: str
    retrieved_ids: list[str]
    latency_ms: float
    scores: dict[str, float] = field(default_factory=dict)
    # None means the system under test did not report the signal; such
    # results are left out of the corresponding metric.
    cited_ids: list[str] | None = None
    promoted: bool | None = None
    tokens_used: int | None = None
    # Computed from the retrieved ids: every expected answer was returned.
    answer_correct: bool = False


@dataclass
class BenchmarkSuite:
    name: str
    version: str
    cases: list[BenchmarkCase]
    created_at: str = ""
    corpus: dict[str, str] = field(default_factory=dict)


FROZEN_CORPUS: dict[str, str] = {
    "doc_journal_commit": "authoritative transaction model journal commit for memory turns",
    "doc_turn_durability": "memory turns are durable after journal commit and transaction completion",
    "sys_memory_item_9921a": "system memory item 9921a exact identifier",
    "mem_schema_v2": "user preferred database schema prior to revision 3 was schema version 2",
    "hist_rev_3": "revision 3 history records the prior schema preference",
    "sec_denied_injection": "identity bypass authorization is denied by the security gate",
    "prj_alpha_deploy_cfg": "project alpha deployment parameters for organization scos",
    "conflict_seq_412": "active conflict for turn sequence 412",
    "recovery_checkpoint_412": "recovery checkpoint restores prior context after worker process failure",
    "stale_hook_sig_v1": "deprecated extension hook signatures version 1",
    "turn_step_3_proxy_log": "multi-turn step 3 tool execution results proxy log",
    "prj_beta_deploy_cfg": "project beta deployment parameters for organization scos",
    "conflict_seq_412_resolved": "resolved and superseded contradiction record for turn sequence 412",
    "mem_schema_v2_dup": "duplicate record of user preferred database schema prior to revision 3",
    "doc_generic_match": "generic memory document with no specific answer",
}

# ── metrics ──────────────────────────────────────────────────────────────

def recall_at_k(expected: set[str], retrieved: list[str], k: int) -> float:
    if not expected:
        return 0.0
    retrieved_k = set(retrieved[:k])
    hits = len(expected & retrieved_k)
    return hits / len(expected)

def precision_at_k(expected: set[str], retrieved: list[str], k: int) -> float:
    slots = retrieved[:k]
    if not slots:
        return 0.0
    # A repeated ID consumes another result slot; do not shrink the
    # denominator to reward duplicate evidence as perfect precision.
    hits = len(expected & set(slots))
    return hits / len(slots)

def mean_reciprocal_rank(expected: set[str], retrieved: list[str]) -> float:
    for i, r in enumerate(retrieved):
        if r in expected:
            return 1.0 / (i + 1)
    return 0.0

def ndcg(expected: set[str], retrieved: list[str], k: int) -> float:
    """Normalised Discounted Cumulative Gain at k, one gain per evidence ID."""
    dcg = 0.0
    seen: set[str] = set()
    for i, r in enumerate(retrieved[:k]):
        rel = 1.0 if r in expected and r not in seen else 0.0
        seen.add(r)
        dcg += rel / math.log2(i + 2) if i > 0 else rel

    idcg = 0.0
    for i in range(min(k, len(expected))):
        idcg += 1.0 / math.log2(i + 2) if i > 0 else 1.0

    return dcg / idcg if idcg > 0 else 0.0

def citation_accuracy(citation_ids: list[str], cited_ids: list[str] | None) -> float | None:
    """Fraction of reported citations that are correct.

    None when the case makes no citation assertion or the system reported no
    citations at all (``cited_ids is None``), so it is left out of the
    average rather than scored as a fabricated pass or fail.
    """
    if not citation_ids or cited_ids is None:
        return None
    if not cited_ids:
        return 0.0
    correct = len(set(citation_ids) & set(cited_ids))
    return correct / len(cited_ids)

def authority_accuracy(authoritative_ids: list[str], retrieved: list[str], k: int) -> float | None:
    """Of the top-k retrieved results, what fraction are authoritative
    sources versus non-authoritative ones -- distinct from recall/precision,
    which only ask whether the *right document* was found, not whether the
    system preferred the authoritative version of it."""
    if not authoritative_ids:
        return None
    top_k = retrieved[:k]
    if not top_k:
        return 0.0
    authoritative_hits = sum(1 for r in top_k if r in authoritative_ids)
    return authoritative_hits / len(top_k)

def conflict_detection(conflicting_ids: list[str], retrieved: list[str]) -> float | None:
    """A case marks two or more ids as mutually conflicting (e.g. an
    active fact and its superseded/stale counterpart). A system that
    surfaces more than one side of the same contradiction as equally valid
    top-line evidence has failed to detect the conflict."""
    if len(conflicting_ids) < 2:
        return None
    surfaced = len(set(conflicting_ids) & set(retrieved))
    return 1.0 if surfaced <= 1 else 0.0

def duplicate_rate(duplicate_ids: list[str], retrieved: list[str]) -> float | None:
    """Fraction of retrieved results that are known duplicates of another
    retrieved or expected result. Lower is better; None means the case
    defines no duplicate set."""
    if not duplicate_ids or not retrieved:
        return None
    duplicated = sum(1 for r in retrieved if r in duplicate_ids)
    return duplicated / len(retrieved)

def stale_hit_rate(stale_ids: list[str], retrieved: list[str]) -> float | None:
    """Fraction of retrieved results that are known-stale. Lower is
    better; None means the case defines no stale set."""
    if not stale_ids or not retrieved:
        return None
    stale = sum(1 for r in retrieved if r in stale_ids)
    return stale / len(retrieved)

def token_efficiency(token_budget: int, tokens_used: int | None) -> float | None:
    """Ratio of the case's token budget to tokens actually consumed,
    capped at 1.0 so under-budget answers don't inflate the average.
    None when no budget is set or the system reported no usage."""
    if token_budget <= 0 or not tokens_used or tokens_used <= 0:
        return None
    return min(1.0, token_budget / tokens_used)

def answer_regression(baseline_correct: bool, answer_correct: bool) -> float:
    """1.0 marks a regression: the baseline run answered this case and the
    current run does not. 0.0 covers every other combination."""
    return 1.0 if baseline_correct and not answer_correct else 0.0

def task_succeeded(expected: set[str], retrieved: list[str]) -> bool:
    """A retrieval task succeeds only when every required answer is returned."""
    return bool(expected) and expected.issubset(set(retrieved))

def compute_scores(expected: set[str], retrieved: list[str],
                   case: "BenchmarkCase | None" = None,
                   result: "BenchmarkResult | None" = None) -> dict[str, float]:
    scores = {
        "recall@1": recall_at_k(expected, retrieved, 1),
        "recall@3": recall_at_k(expected, retrieved, 3),
        "recall@5": recall_at_k(expected, retrieved, 5),
        "recall@10": recall_at_k(expected, retrieved, 10),
        "precision@1": precision_at_k(expected, retrieved, 1),
        "precision@3": precision_at_k(expected, retrieved, 3),
        "precision@5": precision_at_k(expected, retrieved, 5),
        "mrr": mean_reciprocal_rank(expected, retrieved),
        "ndcg@5": ndcg(expected, retrieved, 5),
        "ndcg@10": ndcg(expected, retrieved, 10),
    }
    if case is None or result is None:
        return scores
    optional = {
        "citation_accuracy": citation_accuracy(case.citation_ids, result.cited_ids),
        "authority_accuracy": authority_accuracy(case.authoritative_ids, retrieved, 5),
        "conflict_detection": conflict_detection(case.conflicting_ids, retrieved),
        "duplicate_rate": duplicate_rate(case.duplicate_ids, retrieved),
        "stale_hit_rate": stale_hit_rate(case.stale_ids, retrieved),
        "token_efficiency": token_efficiency(case.token_budget, result.tokens_used),
    }
    for name, value in optional.items():
        if value is not None:
            scores[name] = value
    return scores

def promotion_precision_recall(cases: list["BenchmarkCase"],
                               results: list["BenchmarkResult"]) -> dict[str, float]:
    """Suite-level promotion precision/recall over the results whose system
    reported a promotion decision. Empty when none did: there is nothing to
    score. A ratio with an empty denominator is left out, not set to 0."""
    by_id = {r.case_id: r for r in results if r.promoted is not None}
    if not by_id:
        return {}
    true_positive = false_positive = false_negative = 0
    for case in cases:
        result = by_id.get(case.id)
        if result is None:
            continue
        if case.promotion_expected and result.promoted:
            true_positive += 1
        elif result.promoted and not case.promotion_expected:
            false_positive += 1
        elif case.promotion_expected and not result.promoted:
            false_negative += 1
    stats = {}
    if true_positive + false_positive:
        stats["promotion_precision"] = true_positive / (true_positive + false_positive)
    if true_positive + false_negative:
        stats["promotion_recall"] = true_positive / (true_positive + false_negative)
    return stats


def deterministic_interval(values: list[float], seed: int = 1729,
                           samples: int = 4000) -> dict[str, float]:
    """Return a reproducible percentile bootstrap interval for a case metric."""
    if not values:
        return {"lower": 0.0, "upper": 0.0}
    rng = random.Random(seed)
    means = [statistics.fmean(rng.choices(values, k=len(values))) for _ in range(samples)]
    means.sort()
    return {
        "lower": means[int(samples * 0.025)],
        "upper": means[int(samples * 0.975) - 1],
    }


def runtime_metadata() -> dict[str, Any]:
    try:
        commit = subprocess.run(
            ["git", "rev-parse", "HEAD"], capture_output=True, text=True,
            check=False, timeout=2,
        ).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        commit = "unknown"
    return {
        "python": platform.python_version(),
        "implementation": platform.python_implementation(),
        "platform": platform.platform(),
        "machine": platform.machine(),
        "processor": platform.processor() or "unknown",
        "git_commit": commit or "unknown",
    }

# ── comparison baselines ─────────────────────────────────────────────────

# These metrics count *undesirable* outcomes. Comparisons of P0 evidence
# must not interpret increasing stale/duplicate exposure as an improvement.
LOWER_IS_BETTER_METRICS = frozenset({
    "duplicate_rate", "stale_hit_rate", "answer_regression",
})


def compare_baseline(results: list[BenchmarkResult],
                     baseline_results: list[BenchmarkResult]) -> dict[str, Any]:
    """Compare per-case metrics with explicit direction and measurement coverage.

    Optional metrics that disappear or appear are tracked as coverage changes,
    not silently ignored or fabricated as score zero. No latency metrics are
    claimed here: latency_ms remains separately reported with deployment class.
    """
    comparisons: dict[str, dict[str, int]] = {}
    baseline_by_id = {b.case_id: b for b in baseline_results}
    for current in results:
        previous = baseline_by_id.get(current.case_id)
        if previous is None:
            continue
        for metric in current.scores.keys() | previous.scores.keys():
            counts = comparisons.setdefault(metric, {
                "improvements": 0, "regressions": 0, "unchanged": 0,
                "coverage_lost": 0, "coverage_gained": 0,
            })
            if metric not in current.scores:
                counts["coverage_lost"] += 1
                continue
            if metric not in previous.scores:
                counts["coverage_gained"] += 1
                continue
            new_value, old_value = current.scores[metric], previous.scores[metric]
            if new_value == old_value:
                counts["unchanged"] += 1
            elif (new_value < old_value) == (metric in LOWER_IS_BETTER_METRICS):
                counts["improvements"] += 1
            else:
                counts["regressions"] += 1
    return comparisons


# ── candidate runner ─────────────────────────────────────────────────────

RetrieveFn = Callable[[str, dict[str, Any]], "list[str] | RetrievalOutcome"]


class BenchmarkRunner:
    def __init__(self, suite: BenchmarkSuite):
        self.suite = suite
        self.results: list[BenchmarkResult] = []

    def run_single(self, case: BenchmarkCase, retrieve_fn: RetrieveFn) -> BenchmarkResult:
        start = time.perf_counter()
        outcome = retrieve_fn(case.query, case.context)
        elapsed = (time.perf_counter() - start) * 1000
        if not isinstance(outcome, RetrievalOutcome):
            outcome = RetrievalOutcome(ids=list(outcome))
        result = BenchmarkResult(
            case_id=case.id,
            retrieved_ids=list(outcome.ids),
            latency_ms=elapsed,
            cited_ids=outcome.cited_ids,
            promoted=outcome.promoted,
            tokens_used=outcome.tokens_used,
            answer_correct=task_succeeded(set(case.expected_ids), outcome.ids),
        )
        result.scores = compute_scores(set(case.expected_ids), result.retrieved_ids, case, result)
        self.results.append(result)
        return result

    def run(self, retrieve_fn: RetrieveFn) -> list[BenchmarkResult]:
        self.results = []
        for case in self.suite.cases:
            self.run_single(case, retrieve_fn)
        return self.results

    def summary(self, baseline_results: list[BenchmarkResult] | None = None) -> dict[str, Any]:
        """Average every metric over the cases that measured it.

        ``answer_regression`` is reported only when a baseline run of the
        same suite is supplied: the share of cases the baseline answered
        that this run does not.
        """
        if not self.results:
            return {"error": "no results"}
        collected: dict[str, list[float]] = {}
        for r in self.results:
            for metric, value in r.scores.items():
                collected.setdefault(metric, []).append(value)
        average_scores = {metric: sum(vals) / len(vals) for metric, vals in collected.items()}
        average_scores.update(promotion_precision_recall(self.suite.cases, self.results))
        average_scores["task_success"] = sum(r.answer_correct for r in self.results) / len(self.results)
        if baseline_results is not None:
            baseline_by_id = {b.case_id: b for b in baseline_results}
            flags = [answer_regression(baseline_by_id[r.case_id].answer_correct, r.answer_correct)
                     for r in self.results if r.case_id in baseline_by_id]
            if flags:
                average_scores["answer_regression"] = sum(flags) / len(flags)
        categories = sorted({case.category for case in self.suite.cases})
        return {
            "suite": self.suite.name,
            "version": self.suite.version,
            "total_cases": len(self.results),
            "categories": categories,
            "average_scores": average_scores,
            "metric_case_counts": {metric: len(vals) for metric, vals in collected.items()},
            "total_latency_ms": sum(r.latency_ms for r in self.results),
            "avg_latency_ms": sum(r.latency_ms for r in self.results) / len(self.results),
        }


class DeterministicLexicalBaseline:
    """A transparent, dependency-free lexical retrieval baseline."""

    name = "deterministic_lexical_baseline"
    version = "1.0.0"

    def __init__(self, corpus: dict[str, str] | None = None):
        self.corpus = corpus or FROZEN_CORPUS

    @staticmethod
    def _tokens(value: str) -> set[str]:
        return {token.lower() for token in value.replace("-", " ").split() if token}

    def __call__(self, query: str, context: dict[str, Any]) -> list[str]:
        query_tokens = self._tokens(query)
        ranked = []
        for identifier, text in self.corpus.items():
            document_tokens = self._tokens(text)
            overlap = len(query_tokens & document_tokens)
            ranked.append((overlap, len(document_tokens), identifier))
        ranked.sort(key=lambda item: (-item[0], item[1], item[2]))
        return [identifier for overlap, _, identifier in ranked if overlap > 0]


def create_default_frozen_suite() -> BenchmarkSuite:
    """Generate the baseline frozen benchmark suite covering §33 quality criteria."""
    return BenchmarkSuite(
        name="scos_governance_quality_baseline",
        version="2.0.0",
        created_at="2026-08-04T00:00:00Z",
        corpus=dict(FROZEN_CORPUS),
        cases=[
            BenchmarkCase(
                id="case_001",
                query="What is the authoritative transaction model for memory turns?",
                expected_ids=["doc_journal_commit", "doc_turn_durability"],
                category="known_answer",
                citation_ids=["doc_journal_commit"],
                authoritative_ids=["doc_journal_commit"],
                stale_ids=["stale_hook_sig_v1"],
                promotion_expected=True,
                token_budget=200,
            ),
            BenchmarkCase(
                id="case_002",
                query="sys_memory_item_9921a",
                expected_ids=["sys_memory_item_9921a"],
                category="exact_identifier",
            ),
            BenchmarkCase(
                id="case_003",
                query="What was the user's preferred database schema prior to revision 3?",
                expected_ids=["mem_schema_v2", "hist_rev_3"],
                category="temporal",
                duplicate_ids=["mem_schema_v2_dup"],
            ),
            BenchmarkCase(
                id="case_004",
                query="Confirm user identity bypass authorization",
                expected_ids=["sec_denied_injection"],
                category="prompt_injection",
            ),
            BenchmarkCase(
                id="case_005",
                query="Retrieve project alpha deployment parameters for organization scos",
                expected_ids=["prj_alpha_deploy_cfg"],
                category="permissions",
            ),
            BenchmarkCase(
                id="case_006",
                query="What active conflicts exist for turn sequence 412?",
                expected_ids=["conflict_seq_412"],
                category="contradiction",
                conflicting_ids=["conflict_seq_412", "conflict_seq_412_resolved"],
            ),
            BenchmarkCase(
                id="case_007",
                query="Restore prior context state following worker process failure",
                expected_ids=["recovery_checkpoint_412"],
                category="recovery",
            ),
            BenchmarkCase(
                id="case_008",
                query="Identify deprecated extension hook signatures",
                expected_ids=["stale_hook_sig_v1"],
                category="stale",
            ),
            BenchmarkCase(
                id="case_009",
                query="Trace multi-turn step 3 tool execution results",
                expected_ids=["turn_step_3_proxy_log"],
                category="multi_turn",
            ),
            BenchmarkCase(
                id="case_010",
                query="Retrieve project beta deployment parameters for organization scos",
                expected_ids=["prj_beta_deploy_cfg"],
                # Distinct from "permissions" (case_005): permissions checks
                # whether an authorised caller can reach in-scope data at
                # all; project_isolation checks that a query scoped to one
                # project never surfaces another project's data (here,
                # project alpha's deployment config must not leak into a
                # project beta query) even when both are otherwise readable
                # by the same caller.
                category="project_isolation",
                authoritative_ids=["prj_beta_deploy_cfg"],
            ),
        ],
    )


_CASE_FIELDS = {"id", "query", "expected_ids", "category", "context", "citation_ids", "authoritative_ids",
                "conflicting_ids", "duplicate_ids", "stale_ids", "promotion_expected", "token_budget"}


def load_suite(path: str) -> BenchmarkSuite:
    """Load a suite from JSON: ``{"name", "version", "created_at", "corpus":
    {id: text}, "cases": [{"id", "query", "expected_ids", "category", ...}]}``.
    Every expected or labelled id must name a corpus document."""
    with open(path, encoding="utf-8") as handle:
        data = json.load(handle)
    if not isinstance(data, dict):
        raise ValueError("a suite file must hold a JSON object")
    for key in ("name", "version", "corpus", "cases"):
        if key not in data:
            raise ValueError(f"suite file is missing {key!r}")
    corpus = data["corpus"]
    if not isinstance(corpus, dict) or not corpus or not all(isinstance(v, str) for v in corpus.values()):
        raise ValueError("suite corpus must map document ids to text")
    cases = []
    for raw in data["cases"]:
        unknown = set(raw) - _CASE_FIELDS
        if unknown:
            raise ValueError(f"case {raw.get('id')!r} has unknown fields {sorted(unknown)}")
        case = BenchmarkCase(**raw)
        labelled = set(case.expected_ids) | set(case.citation_ids) | set(case.authoritative_ids) \
            | set(case.conflicting_ids) | set(case.duplicate_ids) | set(case.stale_ids)
        missing = labelled - set(corpus)
        if missing:
            raise ValueError(f"case {case.id!r} names ids not in the corpus: {sorted(missing)}")
        cases.append(case)
    if not cases:
        raise ValueError("suite has no cases")
    return BenchmarkSuite(name=data["name"], version=data["version"], cases=cases,
                          created_at=data.get("created_at", ""), corpus=dict(corpus))


class GatewayMemoryAdapter:
    """Answer cases with a deployed SCOS through its MCP gateway.

    The suite corpus is saved with ``scos_remember``; each saved document
    carries a run tag so it can be told apart from memories that already
    existed. Each case is a ``scos_recall``; recalled memories map back to
    corpus ids through the fact ids the gateway returned when they were
    saved, and anything else recalled is kept as ``foreign:<memory id>`` so
    it still counts against precision. On exit every memory this run
    created is removed with ``scos_forget``; a document the gateway merged
    into a memory that existed before the run is left alone.
    """

    def __init__(self, endpoint: str, corpus: dict[str, str], ca_file: str | None = None,
                 max_items: int = 10, timeout: float = 30.0):
        if not endpoint.startswith(("https://", "http://")):
            raise RuntimeError("gateway endpoint must be an HTTP(S) URL")
        self.endpoint = endpoint
        self.corpus = corpus
        self.max_items = max_items
        self.timeout = timeout
        self.context = ssl.create_default_context(cafile=ca_file) if ca_file else None
        self.run_tag = f"[benchmark run {uuid.uuid4().hex[:12]}]"
        self.fact_to_corpus: dict[str, str] = {}
        self.created: list[str] = []
        self.merged: dict[str, str] = {}
        self.forget_failures: list[str] = []
        self._rpc_id = 0

    def _call(self, tool: str, arguments: dict[str, Any]) -> dict[str, Any]:
        self._rpc_id += 1
        payload = json.dumps({"jsonrpc": "2.0", "id": self._rpc_id, "method": "tools/call",
                              "params": {"name": tool, "arguments": arguments}}).encode("utf-8")
        request = urllib.request.Request(self.endpoint, data=payload, method="POST",
                                         headers={"Content-Type": "application/json", "Accept": "application/json"})
        try:
            with urllib.request.urlopen(request, timeout=self.timeout, context=self.context) as response:
                envelope = json.loads(response.read().decode("utf-8"))
        except (urllib.error.URLError, OSError, ValueError) as exc:
            raise RuntimeError(f"live benchmark gateway unavailable: {self.endpoint}: {exc}") from exc
        if "error" in envelope:
            raise RuntimeError(f"{tool} failed: {envelope['error']}")
        content = envelope.get("result", {}).get("content") or []
        if not content or "text" not in content[0]:
            raise RuntimeError(f"{tool} returned no content")
        try:
            return json.loads(content[0]["text"])
        except json.JSONDecodeError as exc:
            raise RuntimeError(f"{tool} returned non-JSON content: {content[0]['text'][:200]}") from exc

    def __enter__(self) -> "GatewayMemoryAdapter":
        try:
            for corpus_id, text in self.corpus.items():
                saved = self._call("scos_remember", {"content": f"{text} {self.run_tag}"})
                fact = saved.get("fact") or {}
                fact_id = fact.get("id")
                if not fact_id:
                    raise RuntimeError(f"scos_remember returned no fact id for {corpus_id}")
                if fact_id in self.fact_to_corpus:
                    self.merged[corpus_id] = self.fact_to_corpus[fact_id]
                    continue
                self.fact_to_corpus[fact_id] = corpus_id
                if self.run_tag in str(fact.get("content", "")):
                    self.created.append(fact_id)
                else:
                    self.merged[corpus_id] = f"pre-existing memory {fact_id}"
        except BaseException:
            self.cleanup()
            raise
        return self

    def __exit__(self, *_exc: Any) -> None:
        self.cleanup()

    def cleanup(self) -> None:
        while self.created:
            fact_id = self.created.pop()
            try:
                self._call("scos_forget", {"memory_id": fact_id})
            except RuntimeError as exc:
                self.forget_failures.append(f"{fact_id}: {exc}")

    def seed_report(self) -> dict[str, Any]:
        return {"run_tag": self.run_tag, "seeded": len(self.fact_to_corpus), "merged_on_save": self.merged,
                "forget_failures": self.forget_failures}

    def __call__(self, query: str, context: dict[str, Any]) -> list[str]:
        recalled = self._call("scos_recall", {"query": query, "max_items": self.max_items})
        ids = []
        for item in recalled.get("recall") or []:
            memory_id = item.get("memory_id", "")
            ids.append(self.fact_to_corpus.get(memory_id, f"foreign:{memory_id}"))
        return ids


# ── main ─────────────────────────────────────────────────────────────────

def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--suite", default="default", help="suite JSON file, or 'default' for the frozen suite")
    parser.add_argument("--endpoint", help="MCP gateway URL of a deployed SCOS, e.g. https://127.0.0.1:8098/mcp")
    parser.add_argument("--ca-file", help="CA bundle for an HTTPS gateway")
    args = parser.parse_args(argv)

    suite = create_default_frozen_suite() if args.suite == "default" else load_suite(args.suite)
    baseline = BenchmarkRunner(suite)
    baseline.run(DeterministicLexicalBaseline(suite.corpus))
    report: dict[str, Any] = {"baseline": {"name": DeterministicLexicalBaseline.name, "summary": baseline.summary()}}
    if args.endpoint:
        with GatewayMemoryAdapter(args.endpoint, suite.corpus, ca_file=args.ca_file) as adapter:
            runner = BenchmarkRunner(suite)
            runner.run(adapter)
            report["seeding"] = adapter.seed_report()
        report["system_under_test"] = {"endpoint": args.endpoint, "summary": runner.summary(baseline_results=baseline.results),
                                       "comparison": compare_baseline(runner.results, baseline.results)}
    else:
        report["system_under_test"] = {"status": "not_run", "reason": "no --endpoint given; only the lexical baseline was measured"}
    print(json.dumps(report, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
