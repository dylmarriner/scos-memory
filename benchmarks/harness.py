#!/usr/bin/env python3
"""
Benchmark harness (§33 Quality evaluation).

Measures Recall@K, Precision@K, MRR, nDCG, citation accuracy, authority
accuracy, conflict detection, duplicate rate, stale-hit rate, promotion
precision/recall, token efficiency and answer regression.
"""
import json
import math
import platform
import random
import statistics
import subprocess
import sys
import time
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
    # Ids within expected_ids (or the corpus generally) that carry the
    # additional semantics needed by the §33 metrics beyond plain
    # relevance -- which ones must be cited, which are authoritative,
    # which pair off as an active/stale contradiction, which are known
    # duplicates or known-stale, whether this case's memory should be
    # promoted, an approximate token budget, and whether the frozen
    # baseline answered it correctly (for regression tracking).
    citation_ids: list[str] = field(default_factory=list)
    authoritative_ids: list[str] = field(default_factory=list)
    conflicting_ids: list[str] = field(default_factory=list)
    duplicate_ids: list[str] = field(default_factory=list)
    stale_ids: list[str] = field(default_factory=list)
    promotion_expected: bool = False
    token_budget: int = 0
    baseline_correct: bool = True

@dataclass
class BenchmarkResult:
    case_id: str
    retrieved_ids: list[str]
    latency_ms: float
    scores: dict[str, float] = field(default_factory=dict)
    # Populated by callers that track these signals; left empty (and thus
    # scored as "no evidence" rather than a fabricated pass) otherwise.
    cited_ids: list[str] = field(default_factory=list)
    promoted: bool = False
    tokens_used: int = 0
    answer_correct: bool = True

@dataclass
class BenchmarkSuite:
    name: str
    version: str
    cases: list[BenchmarkCase]
    created_at: str = ""


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
    if not retrieved[:k]:
        return 0.0
    retrieved_k = set(retrieved[:k])
    hits = len(expected & retrieved_k)
    return hits / len(retrieved_k)

def mean_reciprocal_rank(expected: set[str], retrieved: list[str]) -> float:
    for i, r in enumerate(retrieved):
        if r in expected:
            return 1.0 / (i + 1)
    return 0.0

def ndcg(expected: set[str], retrieved: list[str], k: int) -> float:
    """Normalised Discounted Cumulative Gain at k."""
    dcg = 0.0
    for i, r in enumerate(retrieved[:k]):
        rel = 1.0 if r in expected else 0.0
        dcg += rel / math.log2(i + 2) if i > 0 else rel

    idcg = 0.0
    for i in range(min(k, len(expected))):
        idcg += 1.0 / math.log2(i + 2) if i > 0 else 1.0

    return dcg / idcg if idcg > 0 else 0.0

def citation_accuracy(citation_ids: list[str], cited_ids: list[str]) -> float | None:
    """Fraction of asserted citations that are actually correct.

    Returns None (not 0.0/1.0) when a case makes no citation assertion, so
    callers can exclude it from the average rather than fabricate a score.
    """
    if not citation_ids:
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

def token_efficiency(token_budget: int, tokens_used: int) -> float | None:
    """Ratio of the case's token budget to tokens actually consumed,
    capped at 1.0 so under-budget answers don't inflate the average.
    None when no budget/usage was tracked for this case."""
    if token_budget <= 0 or tokens_used <= 0:
        return None
    return min(1.0, token_budget / tokens_used)

def answer_regression(baseline_correct: bool, answer_correct: bool) -> float:
    """1.0 marks a regression: the frozen baseline got this case right and
    the current run does not. 0.0 covers every other combination (no
    regression, or a case the baseline never got right to begin with)."""
    return 1.0 if baseline_correct and not answer_correct else 0.0

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
    scores["answer_regression"] = answer_regression(case.baseline_correct, result.answer_correct)
    return scores

def promotion_precision_recall(cases: list["BenchmarkCase"],
                               results: list["BenchmarkResult"]) -> dict[str, float]:
    """Suite-level promotion precision/recall: whether the system promoted
    memory exactly for the cases whose case definition expects promotion.
    This is a corpus-wide measure (needs both true and predicted labels
    across cases), unlike the per-case metrics above."""
    by_id = {r.case_id: r for r in results}
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
    precision = true_positive / (true_positive + false_positive) if (true_positive + false_positive) else 0.0
    recall = true_positive / (true_positive + false_negative) if (true_positive + false_negative) else 0.0
    return {"promotion_precision": precision, "promotion_recall": recall}


def task_succeeded(expected: set[str], retrieved: list[str]) -> bool:
    """A retrieval task succeeds only when every required answer is returned."""
    return bool(expected) and expected.issubset(set(retrieved))


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

def compare_baseline(results: list[BenchmarkResult],
                     baseline_results: list[BenchmarkResult]) -> dict[str, Any]:
    """Compare against a frozen baseline corpus."""
    comparisons = {}
    for r in results:
        if r.case_id not in {b.case_id for b in baseline_results}:
            continue
        base = next(b for b in baseline_results if b.case_id == r.case_id)
        for metric, value in r.scores.items():
            base_val = base.scores.get(metric, 0.0)
            if metric not in comparisons:
                comparisons[metric] = {"improvements": 0, "regressions": 0, "unchanged": 0}
            if value > base_val:
                comparisons[metric]["improvements"] += 1
            elif value < base_val:
                comparisons[metric]["regressions"] += 1
            else:
                comparisons[metric]["unchanged"] += 1
    return comparisons

# ── candidate runner ─────────────────────────────────────────────────────

class BenchmarkRunner:
    def __init__(self, suite: BenchmarkSuite):
        self.suite = suite
        self.results: list[BenchmarkResult] = []

    def run_single(self, case: BenchmarkCase,
                   retrieve_fn: callable) -> BenchmarkResult:
        start = time.time()
        retrieved = retrieve_fn(case.query, case.context)
        elapsed = (time.time() - start) * 1000
        result = BenchmarkResult(
            case_id=case.id,
            retrieved_ids=[r for r in retrieved],
            latency_ms=elapsed,
        )
        result.scores = compute_scores(set(case.expected_ids), result.retrieved_ids, case, result)
        self.results.append(result)
        return result

    def run(self, retrieve_fn: callable) -> list[BenchmarkResult]:
        self.results = []
        for case in self.suite.cases:
            self.run_single(case, retrieve_fn)
        return self.results

    def summary(self) -> dict[str, Any]:
        if not self.results:
            return {"error": "no results"}
        avg_scores: dict[str, float] = {}
        for r in self.results:
            for metric, value in r.scores.items():
                avg_scores.setdefault(metric, []).append(value)
        average_scores = {
            metric: sum(vals) / len(vals)
            for metric, vals in avg_scores.items()
        }
        # Promotion precision/recall is a corpus-wide label comparison, not
        # a per-case score, so it is computed once over the whole suite.
        average_scores.update(promotion_precision_recall(self.suite.cases, self.results))
        categories = sorted({case.category for case in self.suite.cases})
        return {
            "suite": self.suite.name,
            "version": self.suite.version,
            "total_cases": len(self.results),
            "categories": categories,
            "average_scores": average_scores,
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
        version="1.4.0",
        created_at="2026-08-04T00:00:00Z",
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
                baseline_correct=True,
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


class LiveRetrievalAdapter:
    """Production live retrieval adapter interacting with HTTP services or local memory evaluation."""

    def __init__(self, endpoint_url: str = "http://127.0.0.1:8080"):
        self.endpoint_url = endpoint_url.rstrip("/")
        self.last_source = "local_fallback"

    def __call__(self, query: str, context: dict[str, Any]) -> list[str]:
        import urllib.request
        import urllib.parse
        req_payload = json.dumps({"query": query, "context": context}).encode("utf-8")
        req = urllib.request.Request(
            f"{self.endpoint_url}/v1/code/query",
            data=req_payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=3) as resp:
                data = json.loads(resp.read().decode("utf-8"))
                self.last_source = "http"
                if isinstance(data, dict) and "matches" in data:
                    return [m.get("id", str(m)) for m in data["matches"]]
                if isinstance(data, list):
                    return [item.get("id", str(item)) for item in data if isinstance(item, dict)]
        except Exception as exc:
            self.last_source = "unavailable"
            raise RuntimeError(
                f"live benchmark endpoint unavailable: {self.endpoint_url}"
            ) from exc

        self.last_source = "invalid_response"
        raise RuntimeError("live benchmark endpoint returned an unsupported response")


# ── main ─────────────────────────────────────────────────────────────────

def main():
    suite_path = None
    if len(sys.argv) >= 3 and sys.argv[1] == "--suite":
        suite_path = sys.argv[2]

    if suite_path and suite_path != "default":
        suite = load_suite(suite_path)
    else:
        suite = create_default_frozen_suite()

    adapter_url = "http://127.0.0.1:8080"
    if len(sys.argv) >= 5 and sys.argv[3] == "--endpoint":
        adapter_url = sys.argv[4]

    adapter = LiveRetrievalAdapter(adapter_url)
    runner = BenchmarkRunner(suite)
    results = runner.run(adapter)
    summary = runner.summary()
    print(json.dumps(summary, indent=2))
    sys.exit(0)


if __name__ == "__main__":
    main()
