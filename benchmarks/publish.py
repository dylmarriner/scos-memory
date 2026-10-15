#!/usr/bin/env python3
"""Publish measured benchmark runs to the observability service.

The observability service stores each run in its benchmark history
(``POST /v1/benchmarks/runs``) and the transparency dashboard charts it.
Publishing is opt-in: it happens only when ``SCOS_BENCHMARK_PUBLISH_URL``
names the observability base URL. A configured publish that fails raises, so
a run that was meant to be recorded is never silently dropped.
"""
from __future__ import annotations

import json
import os
import ssl
import subprocess
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import Any


def _source_sha() -> str:
    env = os.getenv("SCOS_SOURCE_SHA", "").strip()
    if env:
        return env
    try:
        return subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=True, timeout=5).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def run_from_cases(suite: str, cases: list[dict[str, Any]], *, schema_version: str = "", mode: str = "",
                   wall_time_ms: float = 0.0, recorded_at: str | None = None) -> dict[str, Any]:
    """Build the observability BenchmarkRun payload from measured cases.

    Each case needs ``case_id`` and ``latency_ms``; ``passed`` and numeric
    ``scores``/``metrics`` are carried when present. Totals are left for the
    service to derive from the cases.
    """
    out_cases = []
    for case in cases:
        scores = case.get("scores") or case.get("metrics") or {}
        item: dict[str, Any] = {
            "case_id": str(case["case_id"]),
            "latency_ms": float(case.get("latency_ms", 0.0)),
        }
        numeric = {k: float(v) for k, v in scores.items() if isinstance(v, (int, float)) and not isinstance(v, bool)}
        if numeric:
            item["scores"] = numeric
        if isinstance(case.get("passed"), bool):
            item["passed"] = case["passed"]
        elif isinstance(case.get("task_success"), bool):
            item["passed"] = case["task_success"]
        out_cases.append(item)
    payload: dict[str, Any] = {
        "suite": suite,
        "recorded_at": recorded_at or datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "wall_time_ms": float(wall_time_ms),
        "cases": out_cases,
    }
    if schema_version:
        payload["schema_version"] = schema_version
    if mode:
        payload["mode"] = mode
    sha = _source_sha()
    if sha:
        payload["source_sha"] = sha
    return payload


def _ssl_context() -> ssl.SSLContext | None:
    ca_file = os.getenv("SCOS_TLS_CA_FILE", "").strip()
    if ca_file:
        return ssl.create_default_context(cafile=ca_file)
    return None


def publish(run: dict[str, Any], base_url: str | None = None, token: str | None = None, timeout: float = 15.0) -> dict[str, Any] | None:
    """POST a run to observability. Returns the service response, or None
    when publishing is not configured."""
    base_url = (base_url or os.getenv("SCOS_BENCHMARK_PUBLISH_URL", "")).strip().rstrip("/")
    if not base_url:
        return None
    if not base_url.startswith(("https://", "http://")):
        raise RuntimeError("SCOS_BENCHMARK_PUBLISH_URL must be an HTTP(S) URL")
    token = (token or os.getenv("SCOS_INTERNAL_SERVICE_TOKEN", "")).strip()
    if not token:
        raise RuntimeError("SCOS_INTERNAL_SERVICE_TOKEN is required to publish benchmark runs")
    request = urllib.request.Request(
        base_url + "/v1/benchmarks/runs",
        data=json.dumps(run).encode("utf-8"),
        method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}", "X-SCOS-Caller": "benchmark-harness"},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout, context=_ssl_context()) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "replace")[:500]
        raise RuntimeError(f"benchmark publish failed: HTTP {exc.code}: {detail}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(f"benchmark publish failed: {exc.reason}") from exc
