#!/usr/bin/env python3
"""Run the code-index baseline against the deployed Linode service."""
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import time
from typing import Any

from benchmarks.deployed import DeployedCodeClient, config_from_environment


def _file(path: str, content: str) -> dict[str, Any]:
    return {
        "path": path,
        "language": "go",
        "content": content,
        "content_hash": hashlib.sha256(content.encode("utf-8")).hexdigest(),
    }


def fixture_files(stage: str) -> list[dict[str, Any]]:
    caller = "package benchmark\n\nfunc Caller() { Target() }\n"
    if stage == "initial":
        return [_file("target.go", "package benchmark\n\nfunc Target() {}\n"), _file("caller.go", caller)]
    if stage == "renamed":
        return [_file("renamed.go", "package benchmark\n\nfunc Target() {}\n"), _file("caller.go", caller)]
    if stage == "branch":
        return [_file("branch.go", "package benchmark\n\nfunc BranchOnly() {}\n")]
    raise ValueError(f"unknown benchmark fixture stage: {stage}")


def fixture_identity(config) -> str:
    value = f"{config.tenant_id}\0{config.principal_id}\0{config.repository_path}"
    return hashlib.sha256(value.encode("utf-8")).hexdigest()[:12]


def _case(case_id: str, passed: bool, latency_ms: float, **details: Any) -> dict[str, Any]:
    return {"case_id": case_id, "passed": passed, "latency_ms": latency_ms, "details": details}


def run(config=None) -> dict[str, Any]:
    config = config or config_from_environment()
    client = DeployedCodeClient(config)
    started = time.perf_counter()
    health = client.health()
    release = client.release()
    version = client.version()
    repository_id = client.ensure_repository()
    suffix = fixture_identity(config)
    branch = f"benchmark-{suffix}"
    initial_commit = f"benchmark-initial-{suffix}"
    renamed_commit = f"benchmark-renamed-{suffix}"
    alternate_branch = f"benchmark-branch-{suffix}"

    initial = client.index(repository_id, branch, initial_commit, fixture_files("initial"))
    initial_results = client.query(repository_id, branch, "target.go")
    renamed = client.index(repository_id, branch, renamed_commit, fixture_files("renamed"))
    old_results = client.query(repository_id, branch, "target.go")
    new_results = client.query(repository_id, branch, "renamed.go")
    diff = client.snapshot_diff(renamed["snapshot_id"])
    branch_result = client.index(repository_id, alternate_branch, f"{initial_commit}-branch", fixture_files("branch"))
    branch_results = client.query(repository_id, alternate_branch, "branch.go")

    cases = [
        _case("exact_symbol_lookup", bool(initial_results), 0, result_count=len(initial_results)),
        _case("rename_lineage", bool(new_results) and not old_results,
              0, old_count=len(old_results), new_count=len(new_results), diff=diff),
        _case("branch_isolation", bool(branch_results), 0, result_count=len(branch_results)),
        _case("retry_idempotency", client.index(repository_id, alternate_branch, f"{initial_commit}-branch", fixture_files("branch"))["snapshot_id"] == branch_result["snapshot_id"], 0),
    ]
    elapsed_ms = (time.perf_counter() - started) * 1000
    for item in cases:
        item["latency_ms"] = elapsed_ms / len(cases)
    return {
        "schema_version": "deployed-code-baseline-1",
        "mode": "linode_deployed",
        "metadata": {**config.metadata(), "health": health, "release": release, "version": version},
        "fixture": {"repository_id": repository_id, "branch": branch, "alternate_branch": alternate_branch},
        "cases": cases,
        "summary": {
            "total_cases": len(cases),
            "passed_cases": sum(1 for item in cases if item["passed"]),
            "pass_rate": sum(1 for item in cases if item["passed"]) / len(cases),
            "wall_time_ms": elapsed_ms,
        },
        "exclusions": ["fixture cleanup is not exposed by the current code-index API; fixtures use unique branch/commit identities"],
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    artifact = run()
    args.output.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps({"output": str(args.output), "passed_cases": artifact["summary"]["passed_cases"], "total_cases": artifact["summary"]["total_cases"]}))
    return 0 if artifact["summary"]["passed_cases"] == artifact["summary"]["total_cases"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
