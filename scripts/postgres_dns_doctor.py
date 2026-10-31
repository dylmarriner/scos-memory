#!/usr/bin/env python3
"""Read-only, credential-redacted diagnosis of SCOS Compose PostgreSQL DNS.

Run *on the actual verified Docker host*. This checks network wiring and
optionally resolves 'postgres' from hermes-provider; it does not check
database authentication, application RLS, backups, or production readiness.
It never restarts, creates, removes or updates containers.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
from typing import Any, Callable

PROJECT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$")
CONTAINER_ID = re.compile(r"^[a-fA-F0-9]{12,64}$")
# Docker renders only selected fields. In particular, Config.Env (which can
# contain SCOS_DATABASE_URL and tokens) never crosses the inspection boundary.
INSPECT_FORMAT = (
    '{"Id":{{json .Id}},'
    '"Project":{{json (index .Config.Labels "com.docker.compose.project")}},'
    '"Service":{{json (index .Config.Labels "com.docker.compose.service")}},'
    '"Running":{{json .State.Running}},'
    '"Health":{{with index .State "Health"}}{{json .Status}}{{else}}"not_configured"{{end}},'
    '"Networks":{{json .NetworkSettings.Networks}}}'
)
SERVICES = ("postgres", "hermes-provider")


def _docker(argv: list[str], runner: Callable[..., Any]) -> Any | None:
    """Do not propagate Docker stderr, inspect fields or environment secrets."""
    try:
        return runner(["docker", *argv], capture_output=True, text=True, timeout=10, check=False)
    except (OSError, subprocess.TimeoutExpired):
        return None


def inspect_compose(
    project: str,
    *,
    dns_probe: bool = False,
    runner: Callable[..., Any] = subprocess.run,
) -> dict[str, Any]:
    """Classify only Compose network connectivity, never database health."""
    if not PROJECT.fullmatch(project):
        raise ValueError("invalid Compose project name")

    result: dict[str, Any] = {
        "project": project,
        "network_result": "indeterminate",
        "dns_probe": "not_run",
        "issues": [],
        "services": {},
        "database_readiness_verified": False,
    }
    issues: list[str] = result["issues"]
    ps = _docker(
        ["ps", "--all", "--no-trunc", "--filter", f"label=com.docker.compose.project={project}",
         "--format", "{{.ID}}"],
        runner,
    )
    if ps is None or ps.returncode != 0:
        issues.append("docker_ps_failed")
        return result

    ids = [line.strip() for line in ps.stdout.splitlines() if line.strip()]
    if any(not CONTAINER_ID.fullmatch(container_id) for container_id in ids):
        issues.append("invalid_container_ids")
        return result
    if not ids:
        issues.extend(["postgres_missing", "hermes_provider_missing"])
        result["network_result"] = "fail"
        return result
    inspected = _docker(["inspect", "--format", INSPECT_FORMAT, *ids], runner)
    if inspected is None or inspected.returncode != 0:
        issues.append("docker_inspect_failed")
        return result
    try:
        records = [json.loads(line) for line in inspected.stdout.splitlines() if line.strip()]
    except (ValueError, TypeError):
        issues.append("invalid_docker_inspect_json")
        return result
    if not isinstance(records, list) or len(records) != len(ids):
        issues.append("incomplete_docker_inspect")
        return result

    found: dict[str, dict[str, Any]] = {}
    for record in records:
        if not isinstance(record, dict):
            issues.append("invalid_container_metadata")
            return result
        if record.get("Id") not in ids:
            issues.append("unexpected_container_id")
            return result
        if record.get("Project") != project:
            issues.append("unexpected_project_label")
            return result
        service = record.get("Service")
        if service not in SERVICES:
            continue
        if service in found:
            issues.append("duplicate_" + service.replace("-", "_"))
            result["network_result"] = "fail"
            return result
        found[service] = record

    for service in SERVICES:
        key = service.replace("-", "_")
        record = found.get(service)
        if record is None:
            issues.append(key + "_missing")
            result["services"][service] = {"present": False}
            continue
        networks = record.get("Networks") or {}
        if not isinstance(networks, dict):
            issues.append("invalid_network_metadata")
            return result
        health = record.get("Health", "not_configured")
        running = record.get("Running") is True
        result["services"][service] = {
            "present": True,
            "running": running,
            "health": health if health in ("healthy", "unhealthy", "starting") else "not_configured",
        }
        if not running:
            issues.append(key + "_not_running")
        if health == "unhealthy":
            issues.append(key + "_unhealthy")

    if len(found) != len(SERVICES):
        result["network_result"] = "fail"
        return result
    pg_networks = found["postgres"].get("Networks") or {}
    provider_networks = found["hermes-provider"].get("Networks") or {}
    shared = set(pg_networks).intersection(provider_networks)
    if not shared:
        issues.append("no_shared_network")
    elif not any("postgres" in (pg_networks[name].get("Aliases") or []) for name in shared):
        issues.append("postgres_alias_missing")

    if issues:
        result["network_result"] = "fail"
        return result
    if not dns_probe:
        return result

    # This is the only exec command in this script: read-only DNS resolution.
    provider_id = found["hermes-provider"].get("Id", "")
    if not isinstance(provider_id, str) or not CONTAINER_ID.fullmatch(provider_id):
        issues.append("invalid_provider_container_id")
        return result
    probe = _docker(["exec", provider_id, "getent", "hosts", "postgres"], runner)
    if probe is None or probe.returncode != 0 or not probe.stdout.strip():
        result["dns_probe"] = "failed"
        issues.append("postgres_dns_probe_failed")
        result["network_result"] = "fail"
        return result
    result["dns_probe"] = "resolved"
    result["network_result"] = "pass"
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", default="compose", help="Docker Compose project label (default: compose)")
    parser.add_argument("--dns-probe", action="store_true",
                        help="run read-only getent hosts postgres inside hermes-provider")
    args = parser.parse_args()
    try:
        result = inspect_compose(args.project, dns_probe=args.dns_probe)
    except ValueError as exc:
        parser.error(str(exc))
    print(json.dumps(result, indent=2, sort_keys=True))
    return {"pass": 0, "fail": 2, "indeterminate": 3}[result["network_result"]]


if __name__ == "__main__":
    raise SystemExit(main())
