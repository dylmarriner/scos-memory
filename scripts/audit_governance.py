#!/usr/bin/env python3
"""Fail-closed repository checks for the mandatory governance surface.

This is intentionally a structural gate, not a substitute for integration,
failure-injection, recovery, security, or comparative benchmark evidence.
It prevents documentation and health-only shells from being mistaken for a
complete production implementation.
"""
from __future__ import annotations

from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]

REQUIRED_FILES = {
    "GOVERNANCE.md",
    "api/openapi.yaml",
    "database/migrations/000001_initial.sql",
    "database/migrations/000002_governance_tables.sql",
    "database/migrations/000003_control_plane.sql",
    "database/migrations/000004_autonomous_control_plane.sql",
    "database/migrations/000005_extension_state.sql",
    "database/policies/000001_rls.sql",
    "schemas/events/event-envelope.schema.json",
    "schemas/memory/memory-candidate.schema.json",
    "schemas/routing/route-decision.schema.json",
}

REQUIRED_SERVICES = {
    "memory-gateway",
    "autonomous-control-plane",
    "context-optimisation",
    "code-intelligence",
    "execution-proxy",
    "extension-manager",
    "compatibility-gateway",
    "hermes-provider",
    "memory-intelligence",
    "okf-synchroniser",
    "retrieval-orchestrator",
    "recovery-orchestrator",
    "backup-manager",
    "observability",
    "security-gate",
    "tool-governance",
    "agent-registry",
    "transparency-projection",
}

REQUIRED_INTERFACE_PATHS = {
    "/v1/control/route",
    "/v1/context/compile",
    "/v1/context/restore",
    "/v1/code/query",
    "/v1/code/impact",
    "/v1/proxy/execute",
    "/v1/extensions/install",
    "/v1/transparency/stream",
    "/v1/users/{user_id}/knowledge",
}

FORBIDDEN_MARKERS = re.compile(
    r"\b(?:TODO|FIXME|XXX|not implemented|placeholder|"
    r"simulated|random\.randint)\b",
    re.IGNORECASE,
)


def rel(path: Path) -> str:
    return path.relative_to(ROOT).as_posix()


def main() -> int:
    errors: list[str] = []
    for required_dir in (ROOT / "dashboard", ROOT / "services" / "transparency-projection"):
        if not required_dir.is_dir():
            errors.append(f"missing required directory: {rel(required_dir)}")
    for required in sorted(REQUIRED_FILES):
        if not (ROOT / required).is_file():
            errors.append(f"missing required file: {required}")

    for service in sorted(REQUIRED_SERVICES):
        service_dir = ROOT / "services" / service
        if not (service_dir / "Dockerfile").is_file():
            errors.append(f"missing service Dockerfile: services/{service}")
        if not list((service_dir / "cmd").glob("**/*.go")):
            errors.append(f"missing service implementation: services/{service}")

    migrations = sorted((ROOT / "database/migrations").glob("*.sql"))
    prefixes = [p.name.split("_", 1)[0] for p in migrations]
    if len(prefixes) != len(set(prefixes)):
        errors.append("migration numbering contains duplicate prefixes")

    openapi = (ROOT / "api/openapi.yaml").read_text(encoding="utf-8")
    for path in sorted(REQUIRED_INTERFACE_PATHS):
        if path not in openapi:
            errors.append(f"OpenAPI contract missing required path: {path}")

    excluded = {"GOVERNANCE.md", ".git"}
    for path in ROOT.rglob("*"):
        if not path.is_file() or any(part in excluded for part in path.parts):
            continue
        if rel(path) == "scripts/audit_governance.py":
            continue
        if path.suffix not in {".go", ".py", ".sql", ".yaml", ".yml", ".json", ".md"}:
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        if FORBIDDEN_MARKERS.search(text) and "tests" not in path.parts:
            errors.append(f"incomplete marker in production file: {rel(path)}")

    if errors:
        print("GOVERNANCE AUDIT: FAIL")
        for error in errors:
            print(f"- {error}")
        print(f"{len(errors)} blocking finding(s)")
        return 1

    print("GOVERNANCE AUDIT: PASS (structural gate only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
