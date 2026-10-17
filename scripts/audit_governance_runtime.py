#!/usr/bin/env python3
"""Fail-closed checks for runtime governance evidence.

This is deliberately separate from ``audit_governance.py``.  It checks
executable handlers, stores, middleware wiring, acceptance tests, and evidence
artifacts; file and directory presence is not treated as implementation proof.
"""
from __future__ import annotations

import argparse
import ast
import json
import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
GO_SUFFIXES = {".go"}
PY_SUFFIXES = {".py"}


def relative(root: Path, path: Path) -> str:
    return path.relative_to(root).as_posix()


def production_files(root: Path, suffixes: set[str]) -> list[Path]:
    return sorted(
        path
        for path in root.rglob("*")
        if path.is_file()
        and path.suffix in suffixes
        and not path.name.endswith("_test.go")
        and not path.name.startswith("test_")
        and ".git" not in path.parts
        and "__pycache__" not in path.parts
        and "tests" not in path.parts
    )


def findings_for_pattern(root: Path, pattern: str, message: str) -> list[str]:
    findings: list[str] = []
    regex = re.compile(pattern)
    for path in production_files(root, GO_SUFFIXES):
        for line_number, line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            if regex.search(line):
                findings.append(f"{relative(root, path)}:{line_number}: {message}")
    return findings


def unavailable_handlers(root: Path) -> list[str]:
    return findings_for_pattern(
        root,
        r"(?:StatusNotImplemented|WriteHeader\(\s*501\s*\))",
        "runtime handler returns HTTP 501/unavailable",
    )


def local_json_authority(root: Path) -> list[str]:
    findings: list[str] = []
    # These services explicitly permit process-local/file state instead of the
    # PostgreSQL authority used by the governed deployment.
    services = {
        "services/autonomous-control-plane/cmd/server/main.go": r"DATABASE_URL or SCOS_CONTROL_PLANE_STATE_PATH is required",
        "services/extension-manager/cmd/server/main.go": r"s\.db == nil && s\.storePath == \"\"",
    }
    for name, pattern in services.items():
        path = root / name
        if not path.is_file():
            continue
        regex = re.compile(pattern)
        for line_number, line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            if regex.search(line):
                findings.append(
                    f"{name}:{line_number}: local JSON authority fallback is allowed where PostgreSQL is required"
                )
    return findings


def unconditional_readiness(root: Path) -> list[str]:
    findings: list[str] = []
    patterns = (
        (r'HandleFunc\("/health/ready", healthOK\)', "readiness endpoint is unconditionally healthy"),
        (r'HandleFunc\("/health/ready", readiness\.Handler\(func\(\) error \{ return nil \}\)\)', "readiness endpoint has no runtime dependency check"),
        (r'\{"status":"ready"\}', "readiness endpoint unconditionally reports ready"),
    )
    for pattern, message in patterns:
        findings.extend(findings_for_pattern(root, pattern, message))
    return sorted(set(findings))


# Servers that authenticate every /v1 route themselves rather than through
# the shared middleware, with the reason. Anything else serving /v1 routes
# must wrap its handler in one of the AUTH_WRAPPERS.
SELF_AUTHENTICATING_SERVERS = {
    "services/cloud-control-plane/cmd/server/main.go":
        "public customer ingress: account sessions and API keys are checked per route before any fabric call",
    "integrations/tunnel-client/pkg/localproxy/localproxy.go":
        "vendored upstream OpenAI tunnel-client: a loopback or unix-socket local proxy between the tunnel daemon "
        "and the stdio MCP bridge; it holds no fabric credential, and the bridge authenticates to the gateway",
}
AUTH_WRAPPERS = r"(?:authMiddleware|authenticationMiddleware|RequireAuth|AuthenticatedHandler|ControlPlaneHandler)\s*\("
# Any way a Go program starts serving HTTP: net/http, the shared tlsserve
# package, or an http.Server.
SERVE_CALL = re.compile(r"\bListenAndServe(?:TLS)?\(|\.Serve(?:TLS)?\(")


def missing_auth_middleware(root: Path) -> list[str]:
    findings: list[str] = []
    for path in production_files(root, GO_SUFFIXES):
        text = path.read_text(encoding="utf-8", errors="replace")
        if "/v1/" not in text or not SERVE_CALL.search(text):
            continue
        if relative(root, path) in SELF_AUTHENTICATING_SERVERS:
            continue
        # A direct ServeMux is not middleware, even if individual handlers
        # inspect caller-controlled identity headers.
        if re.search(AUTH_WRAPPERS, text):
            continue
        for line_number, line in enumerate(text.splitlines(), 1):
            if SERVE_CALL.search(line):
                findings.append(
                    f"{relative(root, path)}:{line_number}: HTTP server exposes /v1 routes without authentication middleware"
                )
                break
    return findings


def failure_injection_skips(root: Path) -> list[str]:
    path = root / "tests/failure-injection/runner.py"
    if not path.is_file():
        return ["tests/failure-injection/runner.py: missing failure-injection runner"]
    text = path.read_text(encoding="utf-8", errors="replace")
    # Parse Python as well as using text so a renamed/commented string cannot
    # accidentally satisfy this check.
    try:
        tree = ast.parse(text, filename=str(path))
    except SyntaxError as exc:
        return [f"tests/failure-injection/runner.py:{exc.lineno}: failure-injection runner is not executable Python"]
    has_skipped_status = any(
        isinstance(node, ast.Constant) and node.value == "skipped"
        for node in ast.walk(tree)
    )
    if not has_skipped_status:
        return []
    findings: list[str] = []
    for line_number, line in enumerate(text.splitlines(), 1):
        if "return _result" in line and '"skipped"' in line:
            findings.append(
                f"tests/failure-injection/runner.py:{line_number}: failure-injection check can be skipped instead of executed"
            )
    return findings


def benchmark_evidence(root: Path) -> list[str]:
    benchmark_dir = root / "benchmarks"
    evidence = []
    if benchmark_dir.is_dir():
        for path in benchmark_dir.rglob("*"):
            if not path.is_file() or path.suffix not in {".json", ".jsonl"} or "__pycache__" in path.parts:
                continue
            name = path.name.lower()
            if any(token in name for token in ("result", "report", "evidence", "acceptance")):
                evidence.append(path)
    required_fields = {
        "baseline",
        "results",
        "comparisons",
        "confidence_intervals",
        "task_success",
        "hardware",
    }
    for path in evidence:
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        if isinstance(payload, dict) and required_fields <= payload.keys() and payload["results"]:
            return []
    return [
        "benchmarks: comparative benchmark evidence is absent (no recorded results, baselines, or acceptance report)"
    ]


def audit(root: Path = ROOT) -> list[str]:
    findings: list[str] = []
    findings.extend(unavailable_handlers(root))
    findings.extend(local_json_authority(root))
    findings.extend(unconditional_readiness(root))
    findings.extend(missing_auth_middleware(root))
    findings.extend(failure_injection_skips(root))
    findings.extend(benchmark_evidence(root))
    return sorted(set(findings))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=ROOT, help="repository root (primarily for tests)")
    args = parser.parse_args()
    findings = audit(args.root.resolve())
    if findings:
        print("GOVERNANCE RUNTIME AUDIT: FAIL")
        for finding in findings:
            print(f"- {finding}")
        print(f"{len(findings)} blocking finding(s)")
        return 1
    print("GOVERNANCE RUNTIME AUDIT: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
