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
HARNESS_PATH = pathlib.Path(__file__).resolve().parents[1] / "harness.py"
REPO_ROOT = HARNESS_PATH.parents[1]
METRICS = ("recall@5", "recall@10", "recall@20",
           "precision@5", "precision@10", "precision@20",
           "mrr", "ndcg@5", "ndcg@10", "ndcg@20")
FORBIDDEN_DEPTHS = (5, 10, 20)


def parse_evidence_json(raw: str | bytes) -> dict:
    """Fail closed on ambiguous/non-standard benchmark evidence JSON.

    Default json.loads silently keeps the final duplicate key, including
    nested relevance labels and ranked evidence. It also accepts NaN/Infinity,
    which are not JSON numbers. Neither is valid for a frozen quality gate.
    """
    def unique_keys(pairs: list[tuple[str, object]]) -> dict:
        result: dict = {}
        for key, value in pairs:
            if key in result:
                raise ValueError(f"duplicate JSON key: {key!r}")
            result[key] = value
        return result

    def reject_nonfinite(value: str) -> None:
        raise ValueError(f"non-finite JSON numeric constant: {value}")

    def finite_float(value: str) -> float:
        # RFC 8259 allows exponent notation, but extreme exponents overflow
        # Python floats to infinity without invoking parse_constant.
        number = float(value)
        if not math.isfinite(number):
            raise ValueError(f"non-finite JSON number: {value}")
        return number

    return json.loads(raw, object_pairs_hook=unique_keys,
                      parse_constant=reject_nonfinite, parse_float=finite_float)


def load_fixture(path: pathlib.Path) -> tuple[dict, str]:
    content = path.read_bytes()
    fixture = parse_evidence_json(content)
    if not isinstance(fixture, dict):
        raise ValueError("fixture must be an object")
    documents, cases = fixture.get("corpus"), fixture.get("cases")
    if not isinstance(documents, list) or not documents or not isinstance(cases, list) or not cases:
        raise ValueError("fixture requires nonempty corpus and cases arrays")
    if not isinstance(fixture.get("name"), str) or not fixture["name"].strip():
        raise ValueError("fixture name must be nonempty")

    doc_ids = []
    for document in documents:
        if not isinstance(document, dict) or not isinstance(document.get("id"), str) or not document["id"].strip():
            raise ValueError("document id must be a nonempty string")
        for field in ("fqn", "path", "text"):
            if field in document and not isinstance(document[field], str):
                raise ValueError(f"{document['id']}: document {field} must be a string")
        if not any(document.get(field, "").strip() for field in ("fqn", "path", "text")):
            raise ValueError(f"{document['id']}: document requires searchable content")
        doc_ids.append(document["id"])
    if len(doc_ids) != len(set(doc_ids)):
        raise ValueError("duplicate document id")

    case_ids = []
    known = set(doc_ids)
    for case in cases:
        if not isinstance(case, dict) or not isinstance(case.get("id"), str) or not case["id"].strip():
            raise ValueError("case id must be nonempty")
        case_id = case["id"]
        case_ids.append(case_id)
        if not isinstance(case.get("query"), str) or not case["query"].strip():
            raise ValueError(f"{case_id}: nonempty query required")
        if "requires_semantic" in case and type(case["requires_semantic"]) is not bool:
            raise ValueError(f"{case_id}: requires_semantic must be a boolean")
        expected = case.get("expected_ids")
        forbidden = case.get("forbidden_ids", [])
        for field, values in (("expected_ids", expected), ("forbidden_ids", forbidden)):
            if (not isinstance(values, list) or
                    (field == "expected_ids" and not values) or
                    any(not isinstance(item, str) or not item.strip() for item in values)):
                raise ValueError(f"{case_id}: {field} must contain valid document ids")
            if len(values) != len(set(values)):
                raise ValueError(f"{case_id}: duplicate {field}")
        if set(expected) - known:
            raise ValueError(f"{case_id}: invalid expected ids {sorted(set(expected) - known)}")
        if set(forbidden) - known or set(expected) & set(forbidden):
            raise ValueError(f"{case_id}: invalid forbidden ids")
    if len(case_ids) != len(set(case_ids)):
        raise ValueError("duplicate case id")
    return fixture, hashlib.sha256(content).hexdigest()


def score_ranking(expected: set[str], ranked: list[str]) -> dict[str, float]:
    metrics = {f"recall@{k}": recall_at_k(expected, ranked, k) for k in (5, 10, 20)}
    metrics.update({f"precision@{k}": precision_at_k(expected, ranked, k) for k in (5, 10, 20)})
    metrics["mrr"] = mean_reciprocal_rank(expected, ranked)
    metrics.update({f"ndcg@{k}": ndcg(expected, ranked, k) for k in (5, 10, 20)})
    return metrics


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
        metrics = score_ranking(expected, retrieved)
        results[case["id"]] = {
            "retrieved_ids": retrieved,
            "metrics": metrics,
            "requires_semantic": bool(case.get("requires_semantic", False)),
            **{
                f"forbidden_at_{k}": len(set(retrieved[:k]) & set(case.get("forbidden_ids", [])))
                for k in FORBIDDEN_DEPTHS
            },
        }
    git_sha = subprocess.run(
        ["git", "rev-parse", "HEAD"], cwd=REPO_ROOT,
        text=True, capture_output=True, check=False, timeout=5,
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
        "harness_sha256": hashlib.sha256(HARNESS_PATH.read_bytes()).hexdigest(),
        "deployment_class": "isolated-offline-lexical-smoke",
        "cases": results,
        "average_metrics": {
            metric: statistics.fmean(c["metrics"][metric] for c in results.values())
            for metric in METRICS
        },
        **{
            f"total_forbidden_at_{k}": sum(c[f"forbidden_at_{k}"] for c in results.values())
            for k in FORBIDDEN_DEPTHS
        },
    }


def validate_baseline(fixture: dict, baseline: dict) -> list[str]:
    """Reject inconsistent measured baselines before comparing quality.

    A baseline is evidence, not a configurable quality threshold: stored scores
    must agree with the recorded ranking and frozen fixture labels.
    """
    errors = []
    cases = baseline.get("cases")
    if not isinstance(cases, dict):
        return ["baseline cases must be an object"]
    labels = {case["id"]: case for case in fixture["cases"]}
    if set(cases) != set(labels):
        return ["baseline case set differs from frozen fixture"]
    known_ids = {doc["id"] for doc in fixture["corpus"]}
    for case_id, label in labels.items():
        result = cases[case_id]
        if not isinstance(result, dict):
            errors.append(f"{case_id}: baseline case must be an object")
            continue
        ranked = result.get("retrieved_ids")
        if not isinstance(ranked, list) or any(not isinstance(item, str) for item in ranked):
            errors.append(f"{case_id}: invalid ranked ids")
            continue
        unknown = set(ranked) - known_ids
        if unknown:
            errors.append(f"{case_id}: unknown ranked ids {sorted(unknown)}")
            continue
        if len(ranked) != len(set(ranked)):
            errors.append(f"{case_id}: duplicate ranked ids")
            continue
        measured = score_ranking(set(label["expected_ids"]), ranked)
        stored = result.get("metrics")
        if not isinstance(stored, dict) or set(stored) != set(METRICS):
            errors.append(f"{case_id}: incomplete baseline metrics")
            continue
        for metric in METRICS:
            value = stored[metric]
            if (isinstance(value, bool) or not isinstance(value, (int, float))
                    or not math.isfinite(value) or not math.isclose(
                        value, measured[metric], rel_tol=0, abs_tol=1e-12)):
                errors.append(f"{case_id}: inconsistent baseline {metric}")
        for k in FORBIDDEN_DEPTHS:
            forbidden = len(set(ranked[:k]) & set(label.get("forbidden_ids", [])))
            stored_forbidden = result.get(f"forbidden_at_{k}")
            if (type(stored_forbidden) is not int or stored_forbidden != forbidden):
                errors.append(f"{case_id}: inconsistent forbidden_at_{k}")
        if result.get("requires_semantic") is not bool(label.get("requires_semantic", False)):
            errors.append(f"{case_id}: inconsistent requires_semantic")
    if errors:
        return errors
    aggregates = baseline.get("average_metrics")
    if not isinstance(aggregates, dict) or set(aggregates) != set(METRICS):
        errors.append("invalid average_metrics")
    else:
        for metric in METRICS:
            measured = statistics.fmean(cases[c]["metrics"][metric] for c in cases)
            value = aggregates[metric]
            if (isinstance(value, bool) or not isinstance(value, (int, float))
                    or not math.isfinite(value) or not math.isclose(
                        value, measured, rel_tol=0, abs_tol=1e-12)):
                errors.append(f"inconsistent average_metrics {metric}")
    for k in FORBIDDEN_DEPTHS:
        total = sum(cases[c][f"forbidden_at_{k}"] for c in cases)
        stored_total = baseline.get(f"total_forbidden_at_{k}")
        if type(stored_total) is not int or stored_total != total:
            errors.append(f"inconsistent total_forbidden_at_{k}")
    return errors


def compare(current: dict, baseline: dict) -> list[str]:
    errors = []
    for key in ("schema_version", "fixture_sha256", "fixture_name",
                "engine", "engine_version", "generator_sha256",
                "harness_sha256", "deployment_class"):
        if current[key] != baseline.get(key):
            errors.append(f"{key} mismatch (fixture/engine changed; requalify baseline)")
    if set(current["cases"]) != set(baseline.get("cases", {})):
        errors.append("case set mismatch (missing or extra case)")
        return errors
    for case_id, result in current["cases"].items():
        previous = baseline["cases"][case_id]
        # Quality metrics can be identical after swapping irrelevant evidence.
        # This deterministic engine must preserve its full measured ranking;
        # intentional changes require an explicitly reviewed new baseline.
        if result["retrieved_ids"] != previous.get("retrieved_ids"):
            errors.append(f"{case_id}: deterministic ranking drifted from baseline")
        for metric in METRICS:
            old = previous.get("metrics", {}).get(metric)
            if not isinstance(old, (int, float)) or not math.isfinite(old):
                errors.append(f"{case_id}: missing or invalid baseline {metric}")
            elif result["metrics"][metric] + 1e-12 < old:
                errors.append(f"{case_id}: {metric} regressed from {old} to {result['metrics'][metric]}")
        for k in FORBIDDEN_DEPTHS:
            forbidden = previous.get(f"forbidden_at_{k}")
            if type(forbidden) is not int or forbidden < 0:
                errors.append(f"{case_id}: invalid forbidden_at_{k} exposure threshold")
            elif forbidden != 0:
                errors.append(f"{case_id}: forbidden_at_{k} in baseline is {forbidden}; zero is mandatory")
            if result[f"forbidden_at_{k}"] != 0:
                errors.append(f"{case_id}: forbidden_at_{k} is {result[f'forbidden_at_{k}']}; zero is mandatory")
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
            if any(current[f"total_forbidden_at_{k}"] != 0 for k in FORBIDDEN_DEPTHS):
                raise ValueError("forbidden exposure at k=5/10/20 detected; refusing to record an unsafe baseline")
            args.record_baseline.write_text(json.dumps(current, indent=2, sort_keys=True) + "\n")
            print(f"RECORDED {len(current['cases'])} real lexical retrieval cases to {args.record_baseline}")
            return 0
        baseline = parse_evidence_json(args.check.read_text())
        errors = validate_baseline(fixture, baseline)
        if not errors:
            errors = compare(current, baseline)
        if errors:
            for error in errors:
                print(f"FAIL: {error}", file=sys.stderr)
            return 1
        print(f"PASS: {len(current['cases'])} lexical retrieval cases; "
              f"Recall@5={current['average_metrics']['recall@5']:.3f}; "
              f"forbidden@5/10/20={tuple(current[f'total_forbidden_at_{k}'] for k in FORBIDDEN_DEPTHS)} (fixture-labelled)")
        return 0
    except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
