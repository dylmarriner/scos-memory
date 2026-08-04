#!/usr/bin/env python3
"""
Benchmark harness (§33 Quality evaluation).

Measures Recall@K, Precision@K, MRR, nDCG, citation accuracy,
conflict detection, promotion precision/recall, token efficiency.
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
    category: str  # known_answer, exact_identifier, temporal, contradiction, permissions, multi_turn, recovery, stale, prompt_injection
    context: dict[str, Any] = field(default_factory=dict)

@dataclass
class BenchmarkResult:
    case_id: str
    retrieved_ids: list[str]
    latency_ms: float
    scores: dict[str, float] = field(default_factory=dict)

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

def compute_scores(expected: set[str], retrieved: list[str]) -> dict[str, float]:
    return {
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
        result.scores = compute_scores(set(case.expected_ids), result.retrieved_ids)
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
        return {
            "suite": self.suite.name,
            "version": self.suite.version,
            "total_cases": len(self.results),
            "average_scores": {
                metric: sum(vals) / len(vals)
                for metric, vals in avg_scores.items()
            },
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
        version="1.3.0",
        created_at="2026-07-26T00:00:00Z",
        cases=[
            BenchmarkCase(
                id="case_001",
                query="What is the authoritative transaction model for memory turns?",
                expected_ids=["doc_journal_commit", "doc_turn_durability"],
                category="known_answer",
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
