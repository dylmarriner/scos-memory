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
import math
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


def _finite_measurement(value: Any, *, label: str, nonnegative: bool = False) -> float:
    """Reject non-finite/overflowing measurements before evidence is published."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError(f"benchmark {label} must be a finite number")
    try:
        number = float(value)
    except (TypeError, ValueError, OverflowError) as exc:
        raise ValueError(f"benchmark {label} must be a finite number") from exc
    if not math.isfinite(number) or (nonnegative and number < 0):
        raise ValueError(f"benchmark {label} must be finite and nonnegative" if nonnegative
                         else f"benchmark {label} must be finite")
    return number


def run_from_cases(suite: str, cases: list[dict[str, Any]], *, schema_version: str = "", mode: str = "",
                   wall_time_ms: float = 0.0, recorded_at: str | None = None) -> dict[str, Any]:
    """Build the observability BenchmarkRun payload from measured cases.

    Each case needs ``case_id`` and ``latency_ms``; ``passed`` and numeric
    ``scores``/``metrics`` are carried when present. Totals are left for the
    service to derive from the cases.
    """
    out_cases = []
    seen_case_ids: set[str] = set()
    for case in cases:
        # Never turn missing or non-string identities into apparently valid
        # evidence IDs (e.g. None -> "None" or False -> "False").
        case_id = case.get("case_id")
        if not isinstance(case_id, str) or not case_id.strip():
            raise ValueError("benchmark case_id must be a nonempty string")
        if case_id in seen_case_ids:
            raise ValueError(f"duplicate case_id {case_id!r}")
        seen_case_ids.add(case_id)
        if "latency_ms" not in case:
            raise ValueError("benchmark case latency_ms is required")
        # Validate explicit scores before the fallback: []/0/False/"" must not
        # silently become an empty measurement set.
        scores = case.get("scores")
        if scores is not None and not isinstance(scores, dict):
            raise ValueError("benchmark scores must be an object")
        if not scores:
            scores = case.get("metrics")
            if scores is not None and not isinstance(scores, dict):
                raise ValueError("benchmark scores must be an object")
        scores = scores or {}
        item: dict[str, Any] = {
            "case_id": case_id,
            "latency_ms": _finite_measurement(case["latency_ms"], label="case latency", nonnegative=True),
        }
        numeric = {k: _finite_measurement(v, label="case score") for k, v in scores.items()
                   if isinstance(v, (int, float)) and not isinstance(v, bool)}
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
        "wall_time_ms": _finite_measurement(wall_time_ms, label="wall time", nonnegative=True),
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


_MAX_PUBLISH_ACK_BYTES = 1 << 20


def _reject_json_constant(_value: str) -> None:
    raise ValueError("non-finite JSON constant")


def _reject_duplicate_json_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    """Reject ambiguous acknowledgements, including nested duplicate keys."""
    obj: dict[str, Any] = {}
    for key, value in pairs:
        if key in obj:
            raise ValueError("duplicate JSON key")
        obj[key] = value
    return obj


class _RejectRedirect(urllib.request.HTTPRedirectHandler):
    """Never forward the observability bearer to a redirect destination."""

    def redirect_request(self, request, fp, code, msg, headers, newurl):
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
    try:
        body = json.dumps(run, allow_nan=False).encode("utf-8")
    except (TypeError, ValueError, OverflowError) as exc:
        raise RuntimeError("benchmark publish payload is not valid finite JSON") from exc
    request = urllib.request.Request(
        base_url + "/v1/benchmarks/runs",
        data=body,
        method="POST",
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {token}", "X-SCOS-Caller": "benchmark-harness"},
    )
    opener = urllib.request.build_opener(
        _RejectRedirect(), urllib.request.HTTPSHandler(context=_ssl_context())
    )
    try:
        with opener.open(request, timeout=timeout) as response:
            # The acknowledgement is untrusted. Do not buffer unlimited data or
            # let malformed response bytes escape into benchmark/CI diagnostics.
            raw = response.read(_MAX_PUBLISH_ACK_BYTES + 1)
            if len(raw) > _MAX_PUBLISH_ACK_BYTES:
                raise RuntimeError("invalid benchmark publish acknowledgement: response too large")
            try:
                ack = json.loads(raw.decode("utf-8"), parse_constant=_reject_json_constant,
                                 object_pairs_hook=_reject_duplicate_json_keys)
            except (UnicodeError, ValueError, RecursionError):
                raise RuntimeError("invalid benchmark publish acknowledgement: malformed JSON") from None
            if (not isinstance(ack, dict)
                    or not isinstance(ack.get("run_id"), str)
                    or not 0 < len(ack["run_id"]) <= 128
                    or not ack["run_id"].strip()
                    or not isinstance(ack.get("created"), bool)):
                raise RuntimeError("invalid benchmark publish acknowledgement: missing or invalid fields")
            # The observability API returns 201 only for newly persisted runs
            # and 200 only for an idempotent replay. An inconsistent response
            # cannot establish whether the benchmark was actually recorded.
            if response.status not in (200, 201) or ack["created"] != (response.status == 201):
                raise RuntimeError("invalid benchmark publish acknowledgement: status/created mismatch")
            return ack
    except urllib.error.HTTPError as exc:
        # Upstream error bodies can echo the publisher's Authorization header.
        # Do not read or serialize them into logs, CI, or benchmark artifacts.
        with exc:
            status = exc.code
        # HTTPError.reason is controlled by the remote server and may echo
        # the bearer. Suppress the exception chain in rendered tracebacks.
        raise RuntimeError(f"benchmark publish failed: HTTP {status}") from None
    except urllib.error.URLError as exc:
        raise RuntimeError(f"benchmark publish failed: {exc.reason}") from exc
