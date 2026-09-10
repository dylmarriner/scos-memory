#!/usr/bin/env python3
"""Fail-closed structural gate for the committed benchmark evidence baseline."""
from __future__ import annotations

import json
from pathlib import Path

ROOT = Path(__file__).parent
CORPUS = ROOT / "corpora" / "v1"


def main() -> int:
    manifest = json.loads((CORPUS / "manifest.json").read_text())
    assert manifest["suite"] == "scos-frozen-evidence-baseline"
    assert manifest["version"] == "2026-09-11.v1"
    assert set(manifest["corpora"]) == {"memory", "code", "impact"}
    assert {b["name"] for b in manifest["baselines"]} == {"lexical", "vector", "rrf", "code"}
    assert all(b["deterministic"] for b in manifest["baselines"])
    assert {"recall@1", "precision@1", "mrr", "ndcg@5", "latency_ms", "cost_usd"}.issubset(manifest["metrics"])
    required = set(manifest["claim_policy"]["p4_plus_ranking_claims_require"])
    assert required == {"corpus_version", "git_commit", "model_space", "metrics", "latency_ms", "cost_usd"}
    for domain, spec in manifest["corpora"].items():
        payload = json.loads((CORPUS / spec["path"]).read_text())
        assert payload["schema_version"] == 1 and payload["domain"] == domain
        assert len(payload["records"]) == spec["records"]
        assert len(payload["queries"]) == spec["queries"]
        ids = {record["id"] for record in payload["records"]}
        assert len(ids) == len(payload["records"])
        for query in payload["queries"]:
            assert query["relevant"] and set(query["relevant"]).issubset(ids)
            assert not set(query.get("forbidden", [])) & set(query["relevant"])
    print(f"verified frozen benchmark {manifest['version']}: {sum(s['queries'] for s in manifest['corpora'].values())} queries")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
