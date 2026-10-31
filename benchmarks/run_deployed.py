#!/usr/bin/env python3
"""Run the code-index baseline against a deployed code-intelligence service.

Each case issues its own requests and is timed on its own. Every pass is
decided from what the service returned against expectations the service
never sees. When the deployment is not configured the run is recorded as
not run, with the reason, and exits non-zero: it is never reported as a pass.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import sys
import time
import uuid
from typing import Any, Callable

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from benchmarks.deployed import DeployedCodeClient, config_from_environment  # noqa: E402
from benchmarks.publish import publish, run_from_cases  # noqa: E402

NOT_RUN_EXIT = 2


class DeployedPreflightError(RuntimeError):
    """The configured service could not be qualified before measuring cases."""



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


def _paths(results: list[dict[str, Any]], exact_only: bool = False) -> list[str]:
    return sorted({str(item.get("Path", "")) for item in results
                   if not exact_only or item.get("MatchType") == "exact"})


# A query that names no indexed path falls back to ranked lexical matches,
# which can list any file in the snapshot. Lookups are therefore judged on
# the matches the service declares exact, and isolation on whether a file
# from the other branch or the previous commit appears at all.


def _timed(case_id: str, body: Callable[[], tuple[bool, dict[str, Any]]]) -> dict[str, Any]:
    begin = time.perf_counter()
    try:
        passed, details = body()
    except RuntimeError as exc:
        passed, details = False, {"error": str(exc)}
    return {"case_id": case_id, "passed": passed, "latency_ms": (time.perf_counter() - begin) * 1000, "details": details}


def run(config=None, run_nonce: str | None = None) -> dict[str, Any]:
    config = config or config_from_environment()
    started = time.perf_counter()
    try:
        client = DeployedCodeClient(config)
        health = client.health()
        release = client.release()
        version = client.version()
        repository_id = client.ensure_repository()
    except (RuntimeError, OSError) as exc:
        # Upstream HTTP error bodies can echo bearer credentials. Preserve
        # the cause for a debugger, but never serialize it into artifacts.
        raise DeployedPreflightError("deployed service preflight failed") from exc
    # The fixture identity is stable per benchmark identity; the nonce makes
    # each run's branches and commits new, so a rerun measures indexing
    # again instead of replaying the previous run's snapshots.
    nonce = run_nonce or uuid.uuid4().hex[:8]
    suffix = f"{fixture_identity(config)}-{nonce}"
    branch = f"benchmark-{suffix}"
    alternate_branch = f"benchmark-branch-{suffix}"
    state: dict[str, Any] = {}

    def exact_symbol_lookup():
        state["initial"] = client.index(repository_id, branch, f"benchmark-initial-{suffix}", fixture_files("initial"))
        results = client.query(repository_id, branch, "target.go")
        exact = _paths(results, exact_only=True)
        return exact == ["target.go"], {"exact_paths": exact, "result_paths": _paths(results)}

    def rename_lineage():
        renamed = client.index(repository_id, branch, f"benchmark-renamed-{suffix}", fixture_files("renamed"))
        old_paths = _paths(client.query(repository_id, branch, "target.go"))
        new_exact = _paths(client.query(repository_id, branch, "renamed.go"), exact_only=True)
        diff = client.snapshot_diff(renamed["snapshot_id"])
        renames = sorted({(entry.get("PreviousPath"), entry.get("Path")) for entry in diff.get("Renamed") or []})
        passed = new_exact == ["renamed.go"] and "target.go" not in old_paths and ("target.go", "renamed.go") in renames
        return passed, {"paths_for_old_name": old_paths, "exact_paths_for_new_name": new_exact, "renames": renames}

    def branch_isolation():
        state["branch"] = client.index(repository_id, alternate_branch, f"benchmark-branch-{suffix}", fixture_files("branch"))
        on_branch = _paths(client.query(repository_id, alternate_branch, "branch.go"), exact_only=True)
        main_results = _paths(client.query(repository_id, branch, "BranchOnly"))
        alternate_results = _paths(client.query(repository_id, alternate_branch, "Caller"))
        leaked_to_main = [path for path in main_results if path == "branch.go"]
        leaked_to_branch = [path for path in alternate_results if path in {"caller.go", "renamed.go", "target.go"}]
        passed = on_branch == ["branch.go"] and not leaked_to_main and not leaked_to_branch
        return passed, {"exact_paths_on_alternate_branch": on_branch, "branch_file_on_main": leaked_to_main,
                        "main_files_on_alternate_branch": leaked_to_branch}

    def retry_idempotency():
        if "branch" not in state:
            return False, {"error": "branch_isolation did not index the alternate branch"}
        again = client.index(repository_id, alternate_branch, f"benchmark-branch-{suffix}", fixture_files("branch"))
        return again["snapshot_id"] == state["branch"]["snapshot_id"], {
            "first_snapshot": state["branch"]["snapshot_id"], "retry_snapshot": again["snapshot_id"]}

    cases = [
        _timed("exact_symbol_lookup", exact_symbol_lookup),
        _timed("rename_lineage", rename_lineage),
        _timed("branch_isolation", branch_isolation),
        _timed("retry_idempotency", retry_idempotency),
    ]
    elapsed_ms = (time.perf_counter() - started) * 1000
    passed = sum(1 for item in cases if item["passed"])
    return {
        "schema_version": "deployed-code-baseline-2",
        "status": "measured",
        "mode": "deployed",
        "metadata": {**config.metadata(), "health": health, "release": release, "version": version},
        "fixture": {"repository_id": repository_id, "branch": branch, "alternate_branch": alternate_branch},
        "cases": cases,
        "summary": {
            "total_cases": len(cases),
            "passed_cases": passed,
            "pass_rate": passed / len(cases),
            "wall_time_ms": elapsed_ms,
        },
        "exclusions": ["fixture cleanup is not exposed by the current code-index API; each run uses new branch/commit identities"],
    }


def not_run(reason: str) -> dict[str, Any]:
    return {"schema_version": "deployed-code-baseline-2", "status": "not_run", "reason": reason, "cases": []}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=pathlib.Path, required=True)
    args = parser.parse_args()
    try:
        config = config_from_environment()
    except RuntimeError as exc:
        artifact = not_run(str(exc))
        args.output.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        print(json.dumps({"output": str(args.output), "status": "not_run", "reason": artifact["reason"]}))
        return NOT_RUN_EXIT
    try:
        artifact = run(config)
    except DeployedPreflightError:
        artifact = {
            "schema_version": "deployed-code-baseline-2",
            "status": "failed",
            "stage": "preflight",
            "reason": "deployed service preflight failed; no benchmark cases were measured",
            "metadata": config.metadata(),
            "cases": [],
        }
        args.output.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        print(json.dumps({"output": str(args.output), "status": "failed", "stage": "preflight"}))
        return 1
    args.output.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    published = publish(run_from_cases(
        "deployed-code-baseline", artifact["cases"], schema_version=artifact["schema_version"],
        mode=artifact["mode"], wall_time_ms=artifact["summary"]["wall_time_ms"]))
    if published is not None:
        print(json.dumps({"published_run_id": published.get("run_id")}))
    print(json.dumps({"output": str(args.output), "passed_cases": artifact["summary"]["passed_cases"], "total_cases": artifact["summary"]["total_cases"]}))
    return 0 if artifact["summary"]["passed_cases"] == artifact["summary"]["total_cases"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
