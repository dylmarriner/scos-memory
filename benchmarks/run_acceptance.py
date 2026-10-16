#!/usr/bin/env python3
"""Run the frozen, comparative acceptance programme against the real services.

Every candidate is the production code path:

* context optimisation -- the context-optimisation service binary, over HTTP
  (``/v1/context/compile`` then ``/v1/context/restore``);
* code intelligence -- the code-intelligence service binary with its durable
  PostgreSQL store, over HTTP (``/v2/code/index`` then ``/v2/code/query``);
* execution proxy -- the execution-proxy service binary authorised by the real
  tool-governance service, over HTTP (``/v1/plan``, ``/v1/execute``,
  ``/v1/proxy/restore``);
* control-plane routing -- the autonomous-control-plane binary's ``classify``
  mode, which runs the same route builder that serves ``/v1/control/route``.

The binaries are built from this checkout and run by
``benchmarks.local_services.LocalServiceStack``. Ground truth (expected
values, files, labels, which commands are destructive) stays in this script:
the services receive only the input a real caller would send, and every
metric is computed from what they return. A domain whose service cannot run
here is recorded as not run, with the reason, and is never scored.

``--live`` additionally runs the §33 retrieval suite (``benchmarks/harness.py``)
against a deployed MCP gateway and includes those results in the artifact.
"""
from __future__ import annotations

import argparse
import atexit
import hashlib
import json
import os
import pathlib
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
import uuid
from datetime import datetime, timezone
from typing import Any, Callable

ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from benchmarks.harness import deterministic_interval, runtime_metadata  # noqa: E402
from benchmarks.local_services import (  # noqa: E402
    HTTPError,
    LocalServiceStack,
    StackUnavailable,
    request_json,
    source_sha,
)
from benchmarks.publish import publish, run_from_cases  # noqa: E402


FROZEN_CREATED_AT = "2026-07-27T00:00:00Z"
SUITE_VERSION = "3.1.0"

# Ground-truth corrections and redefinitions, each made independently of any
# candidate's output.
SUITE_REVISIONS = [
    {"revision": "2.1.0", "cases": ["code_002"],
     "reason": "CommandRewrite is defined in both packages/execution-adapters/adapters.go and reducer.py; both are correct answers"},
    {"revision": "2.1.0", "cases": ["route_004", "route_012", "route_017", "route_020", "route_022"],
     "reason": "tool-governance is selected only for tool or command operations (GOVERNANCE.md §21); these requests ask for none"},
    {"revision": "2.1.0", "cases": ["route_016"],
     "reason": "every proxy execution is authorised by tool-governance first, so a proxy route always includes governance"},
    {"revision": "3.0.0", "cases": [f"ctx_{n:03d}" for n in range(1, 25)],
     "reason": "the mandatory-instruction expectation was the constant true; it is now the instruction sentence itself, "
               "which must survive compilation verbatim. Inputs are unchanged"},
    {"revision": "3.0.0", "cases": [f"proxy_{n:03d}" for n in range(1, 21)],
     "reason": "the proxy is now measured on the output of the executed command, not on the command string: "
               "'failure' marks a destructive command that must be blocked without confirmation and is never executed; "
               "'deterministic' marks the safe commands whose raw output can be compared byte for byte with a direct run"},
    {"revision": "3.1.0", "cases": ["proxy_002", "proxy_005", "proxy_007", "proxy_011", "proxy_014", "proxy_019"],
     "reason": "the proxy refuses an unconfirmed destructive command at /v1/execute, where governance's recorded "
               "confirmation is checked; /v1/plan only plans. Blocked now requires the plan to grade the case destructive "
               "and /v1/execute to refuse a destructive command confined to a sentinel directory, which must survive. "
               "The case commands are unchanged and still never executed"},
]

# Token counts in this artifact use one documented tokenizer for input and
# output alike. None of the measured services calls a language model, so
# there is no model tokenizer (and no model cost) to report.
TOKENIZER = {"name": "regex-word-punct-v1", "pattern": r"\w+|[^\w\s]",
             "note": "words and individual punctuation marks; applied identically to input and output"}
_TOKEN_RE = re.compile(TOKENIZER["pattern"])


def count_tokens(text: str) -> int:
    return len(_TOKEN_RE.findall(text))


_CONTEXT_CASES = [
    ("ctx_001", "retain mandatory instruction exact value 412 and cite journal commit", "412", "journal commit"),
    ("ctx_002", "restore source material before answering; preserve error diagnostics and citation", "error diagnostics", "source material"),
    ("ctx_003", "answer using the authoritative deployment configuration and keep project alpha", "project alpha", "deployment configuration"),
    ("ctx_004", "preserve the exact operation id op-7734 recorded by the idempotency ledger", "op-7734", "idempotency ledger"),
    ("ctx_005", "cite the acceptance criteria before promoting the manifest verification result", "manifest verification", "acceptance criteria"),
    ("ctx_006", "quote the governing policy clause forbidding unsigned extension packages", "unsigned extension", "governing policy"),
    ("ctx_007", "reference the retrieval run evidence supporting namespace authorization decision", "namespace authorization", "retrieval run evidence"),
    ("ctx_008", "keep the tool schema contract for operation prepare_operation intact", "prepare_operation", "tool schema contract"),
    ("ctx_009", "preserve the checkpoint identifier ckpt-2290 across the recovery replay", "ckpt-2290", "recovery replay"),
    ("ctx_010", "retain the code symbol OutputReducer.reduce_output from the execution proxy adapter", "OutputReducer.reduce_output", "execution proxy adapter"),
    ("ctx_011", "cite the prior episode where turn sequence 588 was reconciled after crash recovery", "588", "prior episode"),
    ("ctx_012", "honor the confirmation policy blocking unconfirmed destructive tool operation", "destructive tool", "confirmation policy"),
    ("ctx_013", "keep the restoration handle capsule-8842 tied to its authoritative source", "capsule-8842", "authoritative source"),
    ("ctx_014", "cite the security finding cve-slot-9 flagged by the extension signature check", "cve-slot-9", "extension signature check"),
    ("ctx_015", "preserve the failing assertion from the frozen benchmark regression report", "failing assertion", "frozen benchmark regression report"),
    ("ctx_016", "keep the dependency edge between service alpha and namespace beta intact", "service alpha", "dependency edge"),
    ("ctx_017", "cite the rollback plan for migration rev-2044 before applying it", "rev-2044", "rollback plan"),
    ("ctx_018", "retain the capability contract input schema for skill export bundle-11", "bundle-11", "capability contract"),
    ("ctx_019", "preserve the contradiction set flagged between belief-204 and belief-207", "belief-204", "contradiction set"),
    ("ctx_020", "cite the risk score threshold for change impact ticket chg-337", "chg-337", "risk score threshold"),
    ("ctx_021", "keep the retention rule reference for archived diary entry diary-556", "diary-556", "retention rule reference"),
    ("ctx_022", "preserve the reconciliation method declared for operation op-9931", "op-9931", "reconciliation method"),
    ("ctx_023", "cite the capacity budget allocation for context class evidence-slice", "evidence-slice", "capacity budget allocation"),
    ("ctx_024", "keep the supersession chain reference linking version-vi to version-vii", "version-vi", "supersession chain reference"),
]


def frozen_acceptance_cases() -> dict[str, list[dict[str, Any]]]:
    """Return immutable case definitions for each governed capability plane.

    This is a modest, hand-authored corpus, not the large representative
    population §18.10/§19.13/§20.11/§21.18 ultimately require.
    """
    return {
        "context_optimization": [
            # The instruction is repeated five times: the repetition is noise a
            # compiler may remove; the instruction, its exact value and its
            # cited source must survive.
            {"id": case_id, "input": (sentence + " ") * 5,
             "expected": {"instruction": sentence, "exact_value": exact_value, "source": source}}
            for case_id, sentence, exact_value, source in _CONTEXT_CASES
        ],
        "code_intelligence": [
            {"id": "code_001", "query": "OutputReducer.reduce_output", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_002", "query": "CommandRewrite", "expected": ["packages/execution-adapters/adapters.go", "packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_003", "query": "rewrite_command", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_004", "query": "OutputReducer.rewrite_command", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_005", "query": "CommandRewrite.is_rewritten", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_006", "query": "class OutputReducer", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_007", "query": "RequireTLS", "expected": ["packages/database/tls.go"]},
            {"id": "code_008", "query": "packages/execution-adapters/python/scos_execution/reducer.py", "expected": ["packages/execution-adapters/python/scos_execution/reducer.py"]},
            {"id": "code_009", "query": "NonExistentSymbolXYZ", "expected": []},
            {"id": "code_010", "query": "GenerateRestorationKey", "expected": ["packages/context-contracts/context.go"]},
            {"id": "code_011", "query": "ValidateCompileRequest", "expected": ["packages/context-contracts/context.go"]},
            {"id": "code_012", "query": "ValidateManifest", "expected": ["packages/extension-contracts/extensions.go"]},
            {"id": "code_013", "query": "ComputeRecallAtK", "expected": ["packages/benchmark-harness/harness.go"]},
            {"id": "code_014", "query": "ComputeMRR", "expected": ["packages/benchmark-harness/harness.go"]},
            {"id": "code_015", "query": "ComputeNDCGAtK", "expected": ["packages/benchmark-harness/harness.go"]},
            {"id": "code_016", "query": "AuthenticatedHandler", "expected": ["packages/auth/middleware.go"]},
            {"id": "code_017", "query": "ControlPlaneHandler", "expected": ["packages/auth/middleware.go"]},
            {"id": "code_018", "query": "BearerToken", "expected": ["packages/auth/middleware.go"]},
            {"id": "code_019", "query": "GetAdapter", "expected": ["packages/language-adapters/adapters.go"]},
        ],
        # "deterministic" marks a command whose output depends only on the
        # command and the empty workspace it runs in, so its raw result can be
        # compared byte for byte with a direct run. The others read live
        # system state (logs, disk usage, the process table under /proc) that
        # changes between runs.
        "execution_proxy": [
            {"id": "proxy_001", "command": "printf '%s\\n' " + "diagnostic " * 300, "failure": False, "deterministic": True},
            {"id": "proxy_002", "command": "rm -rf /", "failure": True},
            {"id": "proxy_003", "command": "printf '%s\\n' " + "result " * 300, "failure": False, "deterministic": True},
            {"id": "proxy_004", "command": "printf '%s\\n' " + "trace " * 500, "failure": False, "deterministic": True},
            {"id": "proxy_005", "command": "sudo rm -rf /", "failure": True},
            {"id": "proxy_006", "command": "ls -la /var/log", "failure": False, "deterministic": False},
            {"id": "proxy_007", "command": "rm -rf /*", "failure": True},
            {"id": "proxy_008", "command": "cat " + "line " * 400 + "end", "failure": False, "deterministic": True},
            {"id": "proxy_009", "command": "grep -R FIXME_MARKER " + "src/ " * 200, "failure": False, "deterministic": True},
            {"id": "proxy_010", "command": "echo ok", "failure": False, "deterministic": True},
            {"id": "proxy_011", "command": "rm -rf / --no-preserve-root", "failure": True},
            # /usr rather than /: a full-filesystem walk on a host with large
            # data mounts outlasts the request timeout and stalls the programme.
            {"id": "proxy_012", "command": "find /usr -xdev -name '*.so*' " + "-print " * 200, "failure": False, "deterministic": False},
            {"id": "proxy_013", "command": "printf '%s\\n' " + "warn " * 600, "failure": False, "deterministic": True},
            {"id": "proxy_014", "command": "rm -rf / ", "failure": True},
            {"id": "proxy_015", "command": "pwd", "failure": False, "deterministic": True},
            {"id": "proxy_016", "command": "df -h", "failure": False, "deterministic": False},
            {"id": "proxy_017", "command": "curl -s http://internal/health", "failure": False, "deterministic": True},
            {"id": "proxy_018", "command": "tail -n 5000 " + "app.log " * 300, "failure": False, "deterministic": True},
            {"id": "proxy_019", "command": "cat /etc/passwd && rm -rf / ", "failure": True},
            {"id": "proxy_020", "command": "journalctl -u scos " + "-n 1 " * 300, "failure": False, "deterministic": False},
        ],
        "control_plane_routing": [
            {"id": "route_001", "query": "retrieve memory and compile context for this turn", "expected": ["memory", "context"]},
            {"id": "route_002", "query": "find the symbol definition in the repository", "expected": ["code"]},
            {"id": "route_003", "query": "execute the command through the safe proxy", "expected": ["proxy", "governance"]},
            {"id": "route_004", "query": "retrieve memory and route a read-only answer", "expected": ["memory"]},
            {"id": "route_005", "query": "look up the repository symbol and compile fresh context", "expected": ["context", "code"]},
            {"id": "route_006", "query": "run the proxy command safely under governance policy", "expected": ["proxy", "governance"]},
            {"id": "route_007", "query": "retrieve memory context for the interrupted turn and restore state", "expected": ["memory", "context"]},
            {"id": "route_008", "query": "route this read-only request through the safe execution proxy", "expected": ["proxy", "governance"]},
            {"id": "route_009", "query": "find the symbol definition across the repository for this class", "expected": ["code"]},
            {"id": "route_010", "query": "compile context and retrieve memory for workflow continuation", "expected": ["memory", "context"]},
            {"id": "route_011", "query": "block unsafe destructive proxy command without confirmation", "expected": ["proxy", "governance"]},
            {"id": "route_012", "query": "compile a minimal context capsule while keeping the operation safe and non-destructive", "expected": ["context"]},
            {"id": "route_013", "query": "recall the last confirmed memory fact for this user", "expected": ["memory"]},
            {"id": "route_014", "query": "trace symbol references through the repository index", "expected": ["code"]},
            {"id": "route_015", "query": "assemble a compact context capsule for the current task", "expected": ["context"]},
            {"id": "route_016", "query": "issue the execute command against the governed proxy adapter", "expected": ["proxy", "governance"]},
            {"id": "route_017", "query": "review the governance policy before routing a high risk action", "expected": []},
            {"id": "route_018", "query": "restore the checkpoint state and compile the working context", "expected": ["context"]},
            {"id": "route_019", "query": "look up repository history for this prior symbol revision", "expected": ["code"]},
            {"id": "route_020", "query": "retrieve the stored memory item and route it to the caller safely", "expected": ["memory"]},
            {"id": "route_021", "query": "run a read-only command through the proxy without side effects", "expected": ["proxy", "governance"]},
            {"id": "route_022", "query": "audit the routing decision under the current governance policy", "expected": []},
            {"id": "route_023", "query": "compile context for a multi turn conversation and retrieve memory", "expected": ["memory", "context"]},
            {"id": "route_024", "query": "search the repository for the failing symbol definition", "expected": ["code"]},
        ],
    }


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


# ── context optimisation ────────────────────────────────────────────────

class ContextService:
    """The context-optimisation service: compile, then restore from the capsule."""

    def __init__(self, stack: LocalServiceStack):
        self.url = stack.url("context-optimisation")
        self.headers = stack.headers()

    def __call__(self, case: dict[str, Any]) -> dict[str, Any]:
        begin = time.perf_counter()
        compiled = request_json("POST", self.url + "/v1/context/compile", self.headers, {"input": case["input"]})
        compile_ms = (time.perf_counter() - begin) * 1000
        capsule = compiled["capsule"]
        observed: dict[str, Any] = {
            "latency_ms": compile_ms,
            "output": compiled["compiled_text"],
            "capsule_id": capsule.get("capsule_id"),
            "service_reported_tokens": {"original": capsule.get("original_token_count"),
                                        "compiled": capsule.get("compiled_token_count")},
        }
        try:
            begin = time.perf_counter()
            restored = request_json("POST", self.url + "/v1/context/restore", self.headers, {"capsule": capsule})
            observed["restore_latency_ms"] = (time.perf_counter() - begin) * 1000
            observed["restored"] = restored.get("content")
        except HTTPError as exc:
            observed["restored"] = None
            observed["restore_error"] = str(exc)
        return observed


def context_baseline(case: dict[str, Any]) -> dict[str, Any]:
    # The unoptimised baseline sends the input through untouched; it keeps
    # the source itself, so its restoration is the input.
    return {"output": case["input"], "restored": case["input"]}


def context_metrics(case: dict[str, Any], observed: dict[str, Any]) -> dict[str, float]:
    expected = case["expected"]
    output = observed["output"]
    input_tokens, output_tokens = count_tokens(case["input"]), count_tokens(output)
    restored = observed.get("restored")
    metrics = {
        "total_token_reduction": 1 - output_tokens / input_tokens,
        "mandatory_instruction_retention": float(expected["instruction"] in output),
        "exact_value_retention": float(expected["exact_value"] in output),
        "evidence_retention": float(expected["source"] in output),
        "restoration_exact": float(restored is not None and restored.encode("utf-8") == case["input"].encode("utf-8")),
    }
    metrics["task_success"] = float(all(metrics[name] == 1.0 for name in (
        "mandatory_instruction_retention", "exact_value_retention", "evidence_retention", "restoration_exact")))
    observed["input_tokens"], observed["output_tokens"] = input_tokens, output_tokens
    return metrics


# ── code intelligence ───────────────────────────────────────────────────

CODE_CORPUS = ROOT / "packages"


def corpus_files() -> list[pathlib.Path]:
    files = []
    for path in sorted(CODE_CORPUS.rglob("*")):
        rel = path.relative_to(ROOT).as_posix()
        if not path.is_file() or "/tests/" in rel or path.name.startswith("test_") or path.name.endswith("_test.go"):
            continue
        if path.suffix in {".py", ".go"}:
            files.append(path)
    return files


class CodeService:
    """The code-intelligence service's durable store: index the corpus once,
    then answer each case with ``/v2/code/query``."""

    branch = "acceptance"

    def __init__(self, stack: LocalServiceStack):
        self.url = stack.url("code-intelligence")
        self.headers = stack.headers()
        files = []
        for path in corpus_files():
            content = path.read_text(encoding="utf-8")
            files.append({"path": path.relative_to(ROOT).as_posix(),
                          "language": "go" if path.suffix == ".go" else "python",
                          "content": content, "content_hash": _sha256(content.encode("utf-8"))})
        commit = _sha256("".join(f["path"] + f["content_hash"] for f in files).encode("utf-8"))[:40]
        begin = time.perf_counter()
        repo = request_json("POST", self.url + "/v2/code/repos", self.headers, {"canonical_path": "scos-acceptance/packages"})
        self.repository_id = repo["repository_id"]
        indexed = request_json("POST", self.url + "/v2/code/index", self.headers, {
            "repository_id": self.repository_id, "branch": self.branch, "commit_id": commit, "files": files})
        self.setup = {"indexed_files": len(files), "commit_id": commit, "snapshot_id": indexed.get("snapshot_id"),
                      "index_latency_ms": (time.perf_counter() - begin) * 1000}

    def __call__(self, case: dict[str, Any]) -> dict[str, Any]:
        response = request_json("POST", self.url + "/v2/code/query", self.headers, {
            "repository_id": self.repository_id, "branch": self.branch, "query": case["query"]})
        results = response.get("results") or []
        mode = response.get("retrieval_mode", "")
        # Only a match the service itself declares exact is an answer to an
        # exact lookup; ranked lexical/semantic candidates are recorded but
        # are not a claimed definition.
        matches = sorted({item["Path"] for item in results if item.get("MatchType") == "exact"}) if mode == "exact" else []
        return {"matches": matches, "retrieval_mode": mode,
                "top_results": [{"path": item.get("Path"), "fqn": item.get("FullyQualifiedName"),
                                 "match_type": item.get("MatchType")} for item in results[:5]]}


_corpus_text_cache: dict[str, str] | None = None


def code_baseline(case: dict[str, Any]) -> dict[str, Any]:
    """Filename search over the same corpus: a file matches when its name
    contains the query's last path or dotted segment."""
    global _corpus_text_cache
    if _corpus_text_cache is None:
        _corpus_text_cache = {p.relative_to(ROOT).as_posix(): p.read_text(encoding="utf-8") for p in corpus_files()}
    query = case["query"].strip()
    needle = (query.rsplit("/", 1)[-1] if "/" in query else query.split()[-1].rsplit(".", 1)[-1]).lower()
    matches = sorted(rel for rel in _corpus_text_cache if needle and needle in pathlib.PurePosixPath(rel).name.lower())
    return {"matches": matches}


def code_metrics(case: dict[str, Any], observed: dict[str, Any]) -> dict[str, float]:
    expected, matches = set(case["expected"]), set(observed["matches"])
    hits = len(expected & matches)
    metrics = {"exact_lookup": float(matches == expected)}
    if expected:
        metrics["symbol_definition_recall"] = hits / len(expected)
    if matches:
        metrics["reference_precision"] = hits / len(matches)
    metrics["task_success"] = metrics["exact_lookup"]
    return metrics


# ── execution proxy ─────────────────────────────────────────────────────

def _run_shell(command: str, cwd: pathlib.Path, env: dict[str, str], timeout: float = 120) -> dict[str, Any]:
    try:
        done = subprocess.run(["/bin/sh", "-c", command], cwd=cwd, env=env, capture_output=True, timeout=timeout)
        return {"stdout": done.stdout, "stderr": done.stderr, "exit_status": done.returncode}
    except subprocess.TimeoutExpired as exc:
        return {"stdout": exc.stdout or b"", "stderr": exc.stderr or b"", "exit_status": 124}


def _raw_bytes(run: dict[str, Any]) -> bytes:
    return run["stdout"] + b"\x00" + run["stderr"]


def _delivered_text(result: dict[str, Any]) -> str:
    """What the proxy hands back to its caller in place of the raw output."""
    parts = [result.get("stdout_summary", "")]
    for group in result.get("groups") or []:
        parts.append(group.get("heading", ""))
        parts.extend(group.get("items") or [])
    parts.append(result.get("stderr_summary", ""))
    parts.extend(result.get("errors") or [])
    parts.extend(result.get("warnings") or [])
    return "\n".join(part for part in parts if part)


class ProxyService:
    """The execution proxy, authorised by tool-governance.

    A command the frozen case marks destructive is only ever planned, never
    sent to ``/v1/execute``: the plan must grade it destructive, and the
    refusal is measured on a destructive command confined to a sentinel
    directory in the throwaway workspace (see ``_destructive``).
    Every other command is executed through the proxy, its raw result is
    restored from the restoration handle, and both are compared with the
    same command run directly in the same workspace (twice, to tell whether
    its output is reproducible enough to compare byte for byte).
    """

    def __init__(self, stack: LocalServiceStack):
        self.url = stack.url("execution-proxy")
        self.headers = stack.headers()
        self.identity = {"tenant_id": stack.tenant_id, "principal_id": stack.principal_id}
        self.workspace = stack.workspace
        self.env = stack.command_env("execution-proxy")

    def __call__(self, case: dict[str, Any]) -> dict[str, Any]:
        body = {**self.identity, "command": case["command"], "operation_id": str(uuid.uuid4())}
        begin = time.perf_counter()
        if case["failure"]:
            return self._destructive(case, body, begin)
        reference = _run_shell(case["command"], self.workspace, self.env)
        begin = time.perf_counter()
        try:
            result = request_json("POST", self.url + "/v1/execute", self.headers, body)
        except HTTPError as exc:
            return {"latency_ms": (time.perf_counter() - begin) * 1000, "executed": False, "blocked": True,
                    "refusal": {"status": exc.status, "body": exc.body.strip()[:200]}}
        execute_ms = (time.perf_counter() - begin) * 1000
        second = _run_shell(case["command"], self.workspace, self.env)
        observed: dict[str, Any] = {"latency_ms": execute_ms, "executed": True, "blocked": False,
                                    "exit_status": result.get("exit_status"),
                                    "delivered": _delivered_text(result), "reference": reference, "second_reference": second,
                                    "rewritten": result.get("executed_request") != case["command"]}
        try:
            restored = request_json("GET", self.url + "/v1/proxy/restore/" + result["restoration_handle"], self.headers)
            observed["restored"] = {"stdout": restored["stdout"].encode("utf-8"), "stderr": restored["stderr"].encode("utf-8")}
        except (HTTPError, KeyError) as exc:
            observed["restored"] = None
            observed["restore_error"] = str(exc)
        return observed


    def _destructive(self, case: dict[str, Any], body: dict[str, Any], begin: float) -> dict[str, Any]:
        """Blocked means both halves hold. The case's own command is only
        planned (planning runs nothing) and must be graded destructive. The
        refusal itself is measured on /v1/execute with a destructive command
        confined to a sentinel directory in the throwaway workspace: it must
        be refused for want of a recorded confirmation and the sentinel must
        survive. The case's command is never sent to /v1/execute."""
        try:
            plan = request_json("POST", self.url + "/v1/plan", self.headers, body)
        except HTTPError as exc:
            return {"latency_ms": (time.perf_counter() - begin) * 1000, "executed": False, "blocked": False,
                    "plan_error": {"status": exc.status, "body": exc.body.strip()[:200]}}
        graded = plan.get("risk_level") == "destructive"
        sentinel = self.workspace / f"destructive-sentinel-{uuid.uuid4().hex}"
        sentinel.mkdir()
        (sentinel / "keep").write_text("sentinel\n")
        probe = {**self.identity, "command": f"rm -rf {sentinel}", "operation_id": str(uuid.uuid4())}
        refusal: dict[str, Any] | None = None
        try:
            request_json("POST", self.url + "/v1/execute", self.headers, probe)
        except HTTPError as exc:
            refusal = {"status": exc.status, "body": exc.body.strip()[:200]}
        survived = (sentinel / "keep").exists()
        if survived:
            (sentinel / "keep").unlink()
            sentinel.rmdir()
        refused = refusal is not None and refusal["status"] in (400, 403, 409) and "confirmation" in refusal["body"].lower()
        return {"latency_ms": (time.perf_counter() - begin) * 1000, "executed": not survived,
                "blocked": graded and refused and survived,
                "plan_risk_level": plan.get("risk_level"), "probe_refusal": refusal, "sentinel_survived": survived}


class RawBaseline:
    """No proxy: the command's raw output is handed back as is. It has no
    guard, so a destructive command would simply run; it is not executed."""

    def __init__(self, workspace: pathlib.Path, env: dict[str, str]):
        self.workspace, self.env = workspace, env

    def __call__(self, case: dict[str, Any]) -> dict[str, Any]:
        if case["failure"]:
            return {"executed": False, "blocked": False, "note": "no guard; the command would run unconfirmed and was not executed"}
        begin = time.perf_counter()
        first = _run_shell(case["command"], self.workspace, self.env)
        first_ms = (time.perf_counter() - begin) * 1000
        second = _run_shell(case["command"], self.workspace, self.env)
        return {"latency_ms": first_ms, "executed": True, "blocked": False, "exit_status": first["exit_status"],
                "delivered": (first["stdout"] + first["stderr"]).decode("utf-8", "replace"),
                "reference": first, "second_reference": second,
                "restored": {"stdout": first["stdout"], "stderr": first["stderr"]}}


def proxy_metrics(case: dict[str, Any], observed: dict[str, Any]) -> dict[str, float]:
    if case["failure"]:
        blocked = float(observed["blocked"])
        return {"destructive_blocked": blocked, "task_success": blocked}
    if not observed["executed"]:
        # A safe command the proxy refused: nothing was preserved.
        return {"failure_preservation": 0.0, "task_success": 0.0}
    reference, second = observed["reference"], observed["second_reference"]
    reproducible = _raw_bytes(reference) == _raw_bytes(second)
    stable_exit = reference["exit_status"] == second["exit_status"]
    delivered = observed["delivered"]
    # The diagnostics to keep are those of the execution the proxy ran. For a
    # deterministic command that is the direct run's stderr; a live-state
    # command's stderr differs from run to run (find under /proc names
    # transient pids), so its own raw stderr, restored from the proxy, is the
    # only record of that execution.
    diagnostics_source = reference
    if not case["deterministic"] and observed.get("restored") is not None:
        diagnostics_source = observed["restored"]
    stderr_lines = [line for line in diagnostics_source["stderr"].decode("utf-8", "replace").splitlines() if line.strip()]
    exit_preserved = stable_exit and observed["exit_status"] == reference["exit_status"]
    diagnostics_kept = all(line in delivered for line in stderr_lines)
    metrics = {"failure_preservation": float(exit_preserved and diagnostics_kept)}
    restored = observed.get("restored")
    if case["deterministic"] and reproducible:
        metrics["raw_result_restoration"] = float(restored is not None and _raw_bytes(restored) == _raw_bytes(reference))
    raw_run = restored if restored is not None else reference
    raw = raw_run["stdout"] + raw_run["stderr"]
    raw_tokens = count_tokens(raw.decode("utf-8", "replace"))
    if raw_tokens:
        metrics["token_reduction"] = 1 - count_tokens(delivered) / raw_tokens
    metrics["task_success"] = float(metrics["failure_preservation"] == 1.0
                                    and metrics.get("raw_result_restoration", 1.0) == 1.0)
    # Keep the artifact readable: replace raw bytes with their measurements.
    summary: dict[str, Any] = {"reproducible": reproducible, "exit_status": observed["exit_status"],
                               "reference_exit_status": reference["exit_status"], "raw_bytes": len(raw),
                               "raw_tokens": raw_tokens, "delivered_tokens": count_tokens(delivered),
                               "reference_sha256": _sha256(_raw_bytes(reference)),
                               "restored_sha256": _sha256(_raw_bytes(restored)) if restored is not None else None,
                               "stderr_lines": len(stderr_lines), "stderr_lines_delivered": sum(line in delivered for line in stderr_lines)}
    if not case["deterministic"]:
        summary["restoration_excluded"] = "the command reads live system state, so a direct run is no byte-for-byte reference"
    elif not reproducible:
        summary["restoration_excluded"] = "the command's output differed between two direct runs, so no byte comparison is possible"
    for key in ("reference", "second_reference", "restored", "delivered"):
        observed.pop(key, None)
    observed.update(summary)
    return metrics


# ── control-plane routing: the production router ─────────────────────────
#
# The autonomous-control-plane binary's ``classify`` mode runs buildRoute
# (the code that serves /v1/control/route) with no database or network. Each
# case is one request on a long-lived process so per-case latency is
# measured. Memory retrieval and context compilation are mandatory on every
# governed turn (§21.4), so they carry no routing signal and are excluded
# from scoring on both sides.

ROUTE_SYSTEM_LABELS = {
    "code-intelligence": "code",
    "adaptive-execution-proxy": "proxy",
    "tool-governance": "governance",
    "memory-gateway": "memory",
    "context-optimisation": "context",
}
MANDATORY_ROUTE_LABELS = {"memory", "context"}
_router_process: subprocess.Popen | None = None


def _router() -> subprocess.Popen:
    global _router_process
    if _router_process is None:
        binary = os.environ.get("SCOS_CONTROL_PLANE_BIN", "").strip()
        if not binary:
            build_dir = pathlib.Path(tempfile.mkdtemp(prefix="scos-router-"))
            atexit.register(shutil.rmtree, build_dir, True)
            binary = str(build_dir / "autonomous-control-plane")
            try:
                subprocess.run(["go", "build", "-o", binary, "./services/autonomous-control-plane/cmd/server"],
                               cwd=ROOT, check=True, capture_output=True, text=True, timeout=600)
            except (OSError, subprocess.SubprocessError) as exc:
                detail = getattr(exc, "stderr", "") or str(exc)
                raise StackUnavailable(f"cannot build the control-plane router: {detail}") from exc
        _router_process = subprocess.Popen([binary, "classify"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                           stderr=subprocess.PIPE, text=True, bufsize=1)
        atexit.register(_router_process.kill)
    return _router_process


def route_candidate(case: dict[str, Any]) -> dict[str, Any]:
    process = _router()
    process.stdin.write(json.dumps({"interaction_id": case["id"], "input": case["query"]}) + "\n")
    process.stdin.flush()
    line = process.stdout.readline()
    if not line:
        raise RuntimeError("control-plane router exited: " + process.stderr.read())
    decision = json.loads(line)
    systems = [step["system"] for step in decision["selected_plan"]]
    labels = sorted({ROUTE_SYSTEM_LABELS[s] for s in systems if s in ROUTE_SYSTEM_LABELS})
    return {"labels": labels, "intent_labels": decision["intent_labels"], "plan": systems}


def route_baseline(case: dict[str, Any]) -> dict[str, Any]:
    # A plain keyword router: the heuristic the production router replaces.
    query = case["query"].lower()
    labels = []
    if any(word in query for word in ("memory", "retrieve")):
        labels.append("memory")
    if any(word in query for word in ("context", "compile", "restore")):
        labels.append("context")
    if any(word in query for word in ("symbol", "repository")):
        labels.append("code")
    if any(word in query for word in ("execute", "proxy", "command")):
        labels.append("proxy")
    if any(word in query for word in ("safe", "governance", "read-only", "route")):
        labels.append("governance")
    return {"labels": labels}


def route_metrics(case: dict[str, Any], observed: dict[str, Any]) -> dict[str, float]:
    expected = set(case["expected"]) - MANDATORY_ROUTE_LABELS
    actual = set(observed["labels"]) - MANDATORY_ROUTE_LABELS
    # An empty conditional selection is correct exactly when nothing
    # conditional was expected.
    return {"routing_precision": len(expected & actual) / len(actual) if actual else float(not expected),
            "routing_recall": len(expected & actual) / len(expected) if expected else float(not actual),
            "task_success": float(expected == actual)}


# ── measurement and evaluation ──────────────────────────────────────────

METRICS: dict[str, Callable[[dict[str, Any], dict[str, Any]], dict[str, float]]] = {
    "context_optimization": context_metrics,
    "code_intelligence": code_metrics,
    "execution_proxy": proxy_metrics,
    "control_plane_routing": route_metrics,
}


def measure(name: str, domain: str, cases: list[dict[str, Any]], execute: Callable[[dict[str, Any]], dict[str, Any]],
            *, baseline: bool, system: str, setup: dict[str, Any] | None = None) -> dict[str, Any]:
    records = []
    metric_values: dict[str, list[float]] = {}
    started = time.perf_counter()
    for case in cases:
        begin = time.perf_counter()
        observed = execute(case)
        latency_ms = (time.perf_counter() - begin) * 1000
        # A runner that also does reference work for the case (running the
        # command directly, restoring) reports the latency of the system
        # under test itself.
        latency_ms = observed.pop("latency_ms", latency_ms)
        metrics = METRICS[domain](case, observed)
        for metric, value in metrics.items():
            metric_values.setdefault(metric, []).append(value)
        records.append({"case_id": case["id"], "metrics": metrics, "task_success": bool(metrics["task_success"]),
                        "latency_ms": latency_ms, "measured": observed})
    intervals = {metric: deterministic_interval(values, seed=4100 + index)
                 for index, (metric, values) in enumerate(sorted(metric_values.items()))}
    summary = {"average_metrics": {metric: statistics.fmean(values) for metric, values in metric_values.items()},
               "metric_case_counts": {metric: len(values) for metric, values in metric_values.items()},
               "task_success": {"successful_cases": sum(r["task_success"] for r in records), "total_cases": len(records),
                                "rate": statistics.fmean(r["task_success"] for r in records)},
               "total_latency_ms": sum(r["latency_ms"] for r in records),
               "average_latency_ms": statistics.fmean(r["latency_ms"] for r in records),
               "wall_time_ms": (time.perf_counter() - started) * 1000}
    metadata: dict[str, Any] = {"runner": "measured_baseline" if baseline else "measured_service",
                                "baseline": baseline, "system_under_test": system}
    if setup:
        metadata["setup"] = setup
    return {"name": name, "version": SUITE_VERSION, "domain": domain, "metadata": metadata, "cases": records,
            "summary": summary, "confidence_intervals": intervals}


THRESHOLDS = {
    "context_optimization": {"total_token_reduction": (">=", 0.50), "mandatory_instruction_retention": (">=", 0.995),
                             "exact_value_retention": (">=", 0.995), "evidence_retention": (">=", 1.0),
                             "restoration_exact": (">=", 1.0)},
    "code_intelligence": {"exact_lookup": (">=", 0.98), "symbol_definition_recall": (">=", 0.95), "reference_precision": (">=", 0.90)},
    "execution_proxy": {"token_reduction": (">=", 0.70), "failure_preservation": (">=", 1.0), "raw_result_restoration": (">=", 1.0),
                        "destructive_blocked": (">=", 1.0)},
    "control_plane_routing": {"routing_precision": (">=", 0.98), "routing_recall": (">=", 0.98)},
}
MEDIAN_METRICS = {"total_token_reduction", "token_reduction"}


def evaluate(candidate: dict[str, Any], baseline: dict[str, Any]) -> dict[str, Any]:
    domain = candidate["domain"]
    candidate_metrics = candidate["summary"]["average_metrics"]
    baseline_metrics = baseline["summary"]["average_metrics"]
    checks = {}
    for metric, (operator, threshold) in THRESHOLDS[domain].items():
        # Only cases the metric applies to contribute; a metric no case
        # measured is reported as unmeasured and fails its threshold.
        values = [case["metrics"][metric] for case in candidate["cases"] if metric in case["metrics"]]
        if not values:
            checks[metric] = {"operator": operator, "threshold": threshold, "value": None, "cases": 0, "passed": False}
            continue
        value = statistics.median(values) if metric in MEDIAN_METRICS else statistics.fmean(values)
        checks[metric] = {"operator": operator, "threshold": threshold, "value": value, "cases": len(values),
                          "passed": value >= threshold if operator == ">=" else value < threshold}
    deltas = {metric: candidate_metrics[metric] - baseline_metrics[metric]
              for metric in candidate_metrics if metric in baseline_metrics}
    task_deltas = [int(a["task_success"]) - int(b["task_success"])
                   for a, b in zip(candidate["cases"], baseline["cases"])]
    all_thresholds_pass = all(check["passed"] for check in checks.values())
    return {"domain": domain, "status": "measured", "candidate": candidate["name"], "baseline": baseline["name"],
            "thresholds": checks, "all_thresholds_pass": all_thresholds_pass,
            "superiority_claim_permitted": bool(all_thresholds_pass and task_deltas and min(task_deltas) > 0),
            "metric_deltas": deltas, "task_success_delta": statistics.fmean(task_deltas),
            "task_success_delta_confidence_interval": deterministic_interval(task_deltas, seed=9901),
            "latency_ms": {"candidate_average": candidate["summary"]["average_latency_ms"],
                           "baseline_average": baseline["summary"]["average_latency_ms"]}}


DOMAIN_SERVICES = {
    "context_optimization": ["context-optimisation"],
    "code_intelligence": ["code-intelligence"],
    "execution_proxy": ["execution-proxy"],
    "control_plane_routing": [],
}
SYSTEMS = {
    "context_optimization": "context-optimisation service binary over HTTP (/v1/context/compile, /v1/context/restore)",
    "code_intelligence": "code-intelligence service binary with its PostgreSQL store over HTTP (/v2/code/index, /v2/code/query)",
    "execution_proxy": "execution-proxy service binary authorised by the tool-governance service over HTTP "
                       "(/v1/plan, /v1/execute, /v1/proxy/restore)",
    "control_plane_routing": "autonomous-control-plane binary, classify mode (the /v1/control/route builder)",
}
BASELINES = {
    "context_optimization": "unoptimised pass-through of the input",
    "code_intelligence": "file-name search over the same corpus",
    "execution_proxy": "the command run directly, its raw output returned unreduced",
    "control_plane_routing": "keyword router",
}
NAMES = {
    "context_optimization": ("context_optimisation_service", "unoptimised_context_baseline"),
    "code_intelligence": ("code_intelligence_service", "code_filename_baseline"),
    "execution_proxy": ("execution_proxy_service", "raw_output_baseline"),
    "control_plane_routing": ("control_plane_router", "keyword_router_baseline"),
}


def run_domain(domain: str, cases: list[dict[str, Any]]) -> tuple[list[dict[str, Any]], dict[str, Any]]:
    candidate_name, baseline_name = NAMES[domain]
    services = DOMAIN_SERVICES[domain]
    try:
        if not services:
            candidate = measure(candidate_name, domain, cases, route_candidate, baseline=False, system=SYSTEMS[domain])
            baseline = measure(baseline_name, domain, cases, route_baseline, baseline=True, system=BASELINES[domain])
            return [candidate, baseline], evaluate(candidate, baseline)
        with LocalServiceStack(services) as stack:
            setup: dict[str, Any] = {"stack": stack.metadata()}
            if domain == "context_optimization":
                execute, baseline_fn = ContextService(stack), context_baseline
            elif domain == "code_intelligence":
                execute = CodeService(stack)
                setup["index"] = execute.setup
                baseline_fn = code_baseline
            else:
                execute = ProxyService(stack)
                baseline_fn = RawBaseline(stack.workspace, stack.command_env("execution-proxy"))
            candidate = measure(candidate_name, domain, cases, execute, baseline=False, system=SYSTEMS[domain], setup=setup)
            baseline = measure(baseline_name, domain, cases, baseline_fn, baseline=True, system=BASELINES[domain])
            return [candidate, baseline], evaluate(candidate, baseline)
    except StackUnavailable as exc:
        return [], {"domain": domain, "status": "not_run", "candidate": candidate_name, "baseline": baseline_name,
                    "reason": str(exc), "all_thresholds_pass": False, "superiority_claim_permitted": False}


# ── live retrieval evidence ─────────────────────────────────────────────

def run_live(endpoint: str, ca_file: str | None) -> dict[str, Any]:
    """Run the §33 retrieval suite through a deployed MCP gateway. Raises when
    the gateway is unavailable; never substitutes local results."""
    from benchmarks.harness import (BenchmarkRunner, DeterministicLexicalBaseline, GatewayMemoryAdapter,
                                    create_default_frozen_suite)
    suite = create_default_frozen_suite()
    baseline_runner = BenchmarkRunner(suite)
    baseline_runner.run(DeterministicLexicalBaseline(suite.corpus))
    with GatewayMemoryAdapter(endpoint, suite.corpus, ca_file=ca_file) as adapter:
        runner = BenchmarkRunner(suite)
        runner.run(adapter)
        seeding = adapter.seed_report()
    return {"endpoint": endpoint, "system_under_test": "MCP gateway scos_remember/scos_recall/scos_forget",
            "seeding": seeding,
            "summary": runner.summary(baseline_results=baseline_runner.results),
            "baseline": {"name": DeterministicLexicalBaseline.name, "summary": baseline_runner.summary()},
            "cases": [{"case_id": r.case_id, "retrieved_ids": r.retrieved_ids, "latency_ms": r.latency_ms,
                       "scores": r.scores, "task_success": r.answer_correct} for r in runner.results]}


def build_artifact(endpoint: str | None = None, include_live: bool = False, ca_file: str | None = None) -> dict[str, Any]:
    live = None
    if include_live:
        if not endpoint:
            raise RuntimeError("--live requires --endpoint naming the MCP gateway URL")
        live = run_live(endpoint, ca_file)  # raises if the gateway is unavailable
    frozen = frozen_acceptance_cases()
    algorithms, evaluations = [], []
    for domain, cases in frozen.items():
        measured, evaluation = run_domain(domain, cases)
        algorithms.extend(measured)
        evaluations.append(evaluation)
    not_run = [e for e in evaluations if e["status"] == "not_run"]
    exclusions = [f"{e['domain']} not run: {e['reason']}" for e in not_run]
    if live is None:
        exclusions.append("live service evidence not included; use --live --endpoint <MCP gateway URL> to require and measure it")
    exclusions += ["routing scores exclude memory and context, which the router selects on every governed turn",
                   "no measured service calls a language model, so no token cost is reported"]
    artifact: dict[str, Any] = {
        "schema_version": "3.0.0", "generated_at": datetime.now(timezone.utc).isoformat(),
        "source_sha": source_sha(), "baseline": "domain-specific measured baselines",
        "suite": {"name": "scos_native_planes_acceptance", "version": SUITE_VERSION, "frozen_created_at": FROZEN_CREATED_AT,
                  "revisions": SUITE_REVISIONS,
                  "domains": {domain: {"case_count": len(cases), "cases": cases} for domain, cases in frozen.items()}},
        "tokenizer": TOKENIZER,
        "domain_status": {e["domain"]: {"status": e["status"], **({"reason": e["reason"]} if e["status"] == "not_run" else {})}
                          for e in evaluations},
        "algorithms": algorithms, "baselines": [a["name"] for a in algorithms if a["metadata"]["baseline"]],
        "results": [{"algorithm": a["name"], "domain": a["domain"], "cases": a["cases"]} for a in algorithms],
        "comparisons": evaluations, "thresholds": THRESHOLDS,
        "task_success": {a["name"]: a["summary"]["task_success"] for a in algorithms},
        "confidence_intervals": {a["name"]: a["confidence_intervals"] for a in algorithms},
        "hardware": runtime_metadata(),
        "runtime": {"live_requested": include_live, "live_endpoint": endpoint if include_live else None},
        "latency_ms": {a["name"]: a["summary"]["average_latency_ms"] for a in algorithms},
        "exclusions": exclusions,
    }
    if live is not None:
        artifact["live"] = live
    return artifact


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--endpoint", help="MCP gateway URL for --live, e.g. https://127.0.0.1:8098/mcp")
    parser.add_argument("--ca-file", default=os.getenv("SCOS_TLS_CA_FILE") or None,
                        help="CA bundle for an HTTPS gateway (default: SCOS_TLS_CA_FILE)")
    parser.add_argument("--live", action="store_true", help="require the live retrieval suite through the gateway; fail if unavailable")
    parser.add_argument("--output", type=pathlib.Path, default=ROOT / "benchmarks/acceptance-results.json")
    args = parser.parse_args()
    artifact = build_artifact(args.endpoint, args.live, args.ca_file)
    args.output.write_text(json.dumps(artifact, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    for algorithm in artifact["algorithms"]:
        if algorithm["metadata"]["baseline"]:
            continue
        published = publish(run_from_cases(
            f"acceptance:{algorithm['domain']}", algorithm["cases"], schema_version=artifact["schema_version"],
            mode="measured_service", wall_time_ms=algorithm["summary"]["wall_time_ms"], recorded_at=artifact["generated_at"]))
        if published is not None:
            print(json.dumps({"published_run_id": published.get("run_id"), "suite": f"acceptance:{algorithm['domain']}"}))
    if "live" in artifact:
        live_cases = [{"case_id": c["case_id"], "latency_ms": c["latency_ms"], "scores": c["scores"],
                       "passed": c["task_success"]} for c in artifact["live"]["cases"]]
        published = publish(run_from_cases("retrieval:live-gateway", live_cases, schema_version=artifact["schema_version"],
                                           mode="live_gateway", recorded_at=artifact["generated_at"],
                                           wall_time_ms=sum(c["latency_ms"] for c in live_cases)))
        if published is not None:
            print(json.dumps({"published_run_id": published.get("run_id"), "suite": "retrieval:live-gateway"}))
    print(json.dumps({"output": str(args.output), "domains": artifact["domain_status"],
                      "live_included": "live" in artifact}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
