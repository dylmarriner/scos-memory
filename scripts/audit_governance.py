#!/usr/bin/env python3
"""Fail-closed structural checks for the mandatory governance surface.

This gate does not claim runtime, recovery, security, or benchmark compliance.
It only prevents missing contracts and deployment components from passing the
repository validation target.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]

REQUIRED_FILES = {
    "GOVERNANCE.md",
    "README.md",
    "Makefile",
    "api/openapi.yaml",
    "deploy/compose/docker-compose.yml",
    "database/migrations/000001_initial.sql",
    "database/migrations/000002_governance_tables.sql",
    "database/migrations/000003_control_plane.sql",
    "database/migrations/000004_autonomous_control_plane.sql",
    "database/migrations/000005_extension_state.sql",
    "database/migrations/000006_reconcile_routing_decisions.sql",
    "database/policies/000001_rls.sql",
    "database/policies/000002_control_plane_rls.sql",
    "database/policies/000003_data_plane_rls.sql",
    "schemas/events/event-envelope.schema.json",
    "schemas/memory/memory-candidate.schema.json",
    "schemas/routing/route-decision.schema.json",
    "schemas/extensions/extension-manifest.schema.json",
    "schemas/dashboard/transparency-snapshot.schema.json",
    "schemas/handoffs/agent-handoff.schema.json",
}

REQUIRED_DIRECTORIES = {
    "services",
    "workers/consolidation",
    "workers/index-maintenance",
    "workers/policy-evaluation",
    "packages",
    "plugins",
    "skills",
    "hooks",
    "adapters",
    "policies",
    "dashboard",
    "benchmarks",
    "indexes",
    "schemas",
    "database/migrations",
    "database/policies",
    "okf",
    "deploy",
    "tests",
    "docs",
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

# These are the complete HTTP areas listed in GOVERNANCE.md §38. They are
# checked in the published OpenAPI contract, not mistaken for runtime proof.
REQUIRED_INTERFACE_PATHS = {
    "/v1/sessions",
    "/v1/turns",
    "/v1/memory",
    "/v1/okf",
    "/v1/operations",
    "/v1/audit",
    "/v1/retrieval-runs",
    "/v1/control/ingest",
    "/v1/control/route",
    "/v1/control/decisions",
    "/v1/control/replay",
    "/v1/control/recover",
    "/v1/capabilities",
    "/v1/triggers",
    "/v1/context/compile",
    "/v1/context/restore",
    "/v1/context/evaluate",
    "/v1/code/index",
    "/v1/code/query",
    "/v1/code/impact",
    "/v1/proxy/execute",
    "/v1/proxy/restore",
    "/v1/proxy/explain",
    "/v1/benchmarks",
    "/v1/extensions",
    "/v1/extensions/install",
    "/v1/extensions/{package_id}/enable",
    "/v1/extensions/{package_id}/disable",
    "/v1/extensions/{package_id}/rollback",
    "/v1/extensions/{package_id}",
    "/v1/skills",
    "/v1/hooks",
    "/v1/hooks/executions",
    "/v1/adapters",
    "/v1/compatibility",
    "/v1/entities",
    "/v1/entities/{entity_id}/representation",
    "/v1/users/{user_id}/knowledge",
    "/v1/memory/graph",
    "/v1/beliefs",
    "/v1/learnings",
    "/v1/diary",
    "/v1/transparency/snapshot",
    "/v1/transparency/events",
    "/v1/transparency/stream",
    "/health/live",
    "/health/ready",
}

FORBIDDEN_MARKERS = re.compile(
    r"\b(?:TODO|FIXME|XXX|not implemented|placeholder|simulated|random\.randint)\b",
    re.IGNORECASE,
)
CODE_SUFFIXES = {".go", ".py", ".sql", ".yaml", ".yml", ".json"}


def relative(path: Path) -> str:
    return path.relative_to(ROOT).as_posix()


def compose_services(text: str) -> set[str]:
    """Read top-level Compose service names without requiring PyYAML."""
    in_services = False
    services: set[str] = set()
    for line in text.splitlines():
        if line == "services:":
            in_services = True
            continue
        if in_services and line and not line.startswith(" "):
            break
        if in_services:
            match = re.fullmatch(r"  ([A-Za-z0-9][A-Za-z0-9_-]*):", line)
            if match:
                services.add(match.group(1))
    return services


def audit() -> list[str]:
    errors: list[str] = []

    for required_dir in sorted(REQUIRED_DIRECTORIES):
        if not (ROOT / required_dir).is_dir():
            errors.append(f"missing required directory: {required_dir}")
    for required_file in sorted(REQUIRED_FILES):
        path = ROOT / required_file
        if not path.is_file():
            errors.append(f"missing required file: {required_file}")

    # Services no longer carry a Dockerfile each -- deploy/Dockerfile builds
    # every binary into one image and compose selects between them with
    # `command:`. The invariant worth auditing is unchanged, though: every
    # required service must actually be built and shipped. That now means being
    # named in that Dockerfile's SERVICES list.
    fabric_dockerfile = ROOT / "deploy/Dockerfile"
    built_services: set[str] = set()
    if fabric_dockerfile.is_file():
        text = fabric_dockerfile.read_text(encoding="utf-8")
        match = re.search(r'ARG SERVICES="(?P<list>[^"]*)"', text)
        if match:
            built_services = set(match.group("list").split())
    else:
        errors.append("missing required file: deploy/Dockerfile")

    for service in sorted(REQUIRED_SERVICES):
        service_dir = ROOT / "services" / service
        if service not in built_services:
            errors.append(f"service not built by deploy/Dockerfile: {service}")
        if not list((service_dir / "cmd").glob("**/*.go")):
            errors.append(f"missing service implementation: services/{service}")

    migrations = sorted((ROOT / "database/migrations").glob("*.sql"))
    prefixes = [path.name.split("_", 1)[0] for path in migrations]
    if len(prefixes) != len(set(prefixes)):
        errors.append("migration numbering contains duplicate prefixes")

    compose_path = ROOT / "deploy/compose/docker-compose.yml"
    if compose_path.is_file():
        compose = compose_path.read_text(encoding="utf-8")
        missing_services = REQUIRED_SERVICES - compose_services(compose)
        for service in sorted(missing_services):
            errors.append(f"Compose file missing required service: {service}")

    openapi_path = ROOT / "api/openapi.yaml"
    if openapi_path.is_file():
        openapi = openapi_path.read_text(encoding="utf-8")
        for path in sorted(REQUIRED_INTERFACE_PATHS):
            if not re.search(rf"^  {re.escape(path)}:", openapi, re.MULTILINE):
                errors.append(f"OpenAPI contract missing required path: {path}")

    for path in ROOT.rglob("*"):
        if (
            not path.is_file()
            or path.suffix not in CODE_SUFFIXES
            or ".git" in path.parts
            or ".github" in path.parts
            or "node_modules" in path.parts
            or "vendor" in path.parts
            or "dist" in path.parts
            or "build" in path.parts
            or "tests" in path.parts
            # Go tests commonly live alongside production packages rather
            # than in a tests/ directory; they are not production artifacts
            # and may legitimately contain marker strings in assertions.
            or path.name.endswith("_test.go")
            or "__pycache__" in path.parts
            or relative(path) == "scripts/audit_governance.py"
        ):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        if FORBIDDEN_MARKERS.search(text):
            errors.append(f"incomplete marker in production file: {relative(path)}")

    return errors


def main() -> int:
    errors = audit()
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
