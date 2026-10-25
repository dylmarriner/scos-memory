#!/usr/bin/env python3
"""Reproducible *lexical* retrieval regression gate for the P0/P4 smoke corpus.

This executes the real deterministic lexical retriever. It does not run BGE,
FTS, RRF or a deployed SCOS instance, and its timings are not production SLOs.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import pathlib
import statistics
import subprocess
import sys

from benchmarks.harness import (
    DeterministicLexicalBaseline,
    mean_reciprocal_rank,
    ndcg,
    precision_at_k,
    recall_at_k,
)

DEFAULT_FIXTURE = pathlib.Path(__file__).with_name("retrieval_fixture.json")
DEFAULT_BASELINE = pathlib.Path(__file__).with_name("lexical_baseline.json")
METRICS = ("recall@5", "recall@10", "recall@20",
           "precision@5", "precision@10", "precision@20",
           "mrr", "ndcg@5", "ndcg@10", "ndcg@20")


def load_fixture(path: pathlib.Path) -> tuple[dict, str]:
    content = path.read_bytes()
    fixture = json.loads(content)
    if not isinstance(fixture, dict) or not fixture.get("corpus") or not fixture.get("cases"):
        raise ValueError("fixture requires nonempty corpus and cases")
    documents = fixture["corpus"]
    cases = fixture["cases"]
    if not isinstance(documents, list) or not isinstance(cases, list):
        raise ValueError("fixture corpus and cases must be arrays")
    doc_ids = [d["id"] for d in documents]
    case_ids = [c["id"] for c in cases]
    if len(doc_ids) != len(set(doc_ids)) or len(case_ids) != len(set(case_ids)):
        raise ValueError("duplicate document or case id")
    known = set(doc_ids)
    for case in cases:
        expected = case["expected_ids"]
        forbidden = case.get("forbidden_ids", [])
        if not expected or set(expected) - known:
            raise ValueError(f"{case['id']}: invalid expected ids {sorted(set(expected) - known)}")
        if set(forbidden) - known or set(expected) & set(forbidden):
            raise ValueError(f"{case['id']}: invalid forbidden ids")
    return fixture, hashlib.sha256(content).hexdigest()


def run_retrieval(fixture: dict, fixture_sha: str) -> dict:
    corpus = {
        d["id"]: " ".join(str(d.get(k, "")) for k in ("fqn", "path", "text"))
        for d in fixture["corpus"]
    }
    retriever = DeterministicLexicalBaseline(corpus)
    results = {}
    for case in fixture["cases"]:
        retrieved = retriever(case["query"], {})
        expected = set(case["expected_ids"])
        metrics = {
            f"recall@{k}": recall_at_k(expected, retrieved, k)
            for k in (5, 10, 20)
        }
        metrics.update({
            f"precision@{k}": precision_at_k(expected, retrieved, k)
            for k in (5, 10, 20)
        })
        metrics["mrr"] = mean_reciprocal_rank(expected, retrieved)
        metrics.update({f"ndcg@{k}": ndcg(expected, retrieved, k) for k in (5, 10, 20)})
        results[case["id"]] = {
            "retrieved_ids": retrieved,
            "metrics": metrics,
            "forbidden_at_5": len(set(retrieved[:5]) & set(case.get("forbidden_ids", []))),
            "requires_semantic": bool(case.get("requires_semantic", False)),
        }
    git_sha = subprocess.run(
        ["git", "rev-parse", "HEAD"], text=True, capture_output=True, check=False,
        timeout=5,
    ).stdout.strip() or "unknown"
    return {
        "schema_version": 1,
        "fixture_sha256": fixture_sha,
        "fixture_name": fixture["name"],
        "engine": retriever.name,
        "engine_version": retriever.version,
        # This is the checked-out base commit, NOT a claim that uncommitted
        # benchmark changes already exist in that Git commit.
        "git_base_sha": git_sha,
        "generator_sha256": hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest(),
        "deployment_class": "isolated-offline-lexical-smoke",
        "cases": results,
        "average_metrics": {
            metric: statistics.fmean(c["metrics"][metric] for c in results.values())
            for metric in METRICS
        },
        "total_forbidden_at_5": sum(c["forbidden_at_5"] for c in results.values()),
    }


def compare(current: dict, baseline: dict) -> list[str]:
    errors = []
    for key in ("schema_version", "fixture_sha256", "fixture_name",
                "engine", "engine_version", "generator_sha256", "deployment_class"):
        if current[key] != baseline.get(key):
            errors.append(f"{key} mismatch (fixture/engine changed; requalify baseline)")
    if set(current["cases"]) != set(baseline.get("cases", {})):
        errors.append("case set mismatch (missing or extra case)")
        return errors
    for case_id, result in current["cases"].items():
        previous = baseline["cases"][case_id]
        for metric in METRICS:
            old = previous.get("metrics", {}).get(metric)
            if not isinstance(old, (int, float)) or not math.isfinite(old):
                errors.append(f"{case_id}: missing or invalid baseline {metric}")
            elif result["metrics"][metric] + 1e-12 < old:
                errors.append(f"{case_id}: {metric} regressed from {old} to {result['metrics'][metric]}")
        forbidden = previous.get("forbidden_at_5")
        if not isinstance(forbidden, int) or forbidden < 0:
            errors.append(f"{case_id}: invalid forbidden exposure threshold")
        elif result["forbidden_at_5"] > forbidden:
            errors.append(f"{case_id}: forbidden exposure increased from {forbidden} to {result['forbidden_at_5']}")
    return errors


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixture", type=pathlib.Path, default=DEFAULT_FIXTURE)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--record-baseline", type=pathlib.Path)
    group.add_argument("--check", type=pathlib.Path)
    args = parser.parse_args(argv)
    try:
        fixture, digest = load_fixture(args.fixture)
        current = run_retrieval(fixture, digest)
        if args.record_baseline:
            args.record_baseline.write_text(json.dumps(current, indent=2, sort_keys=True) + "\n")
            print(f"RECORDED {len(current['cases'])} real lexical retrieval cases to {args.record_baseline}")
            return 0
        baseline = json.loads(args.check.read_text())
        errors = compare(current, baseline)
        if errors:
            for error in errors:
                print(f"FAIL: {error}", file=sys.stderr)
            return 1
        print(f"PASS: {len(current['cases'])} lexical retrieval cases; "
              f"Recall@5={current['average_metrics']['recall@5']:.3f}; "
              f"forbidden@5={current['total_forbidden_at_5']} (baseline-relative)")
        return 0
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
