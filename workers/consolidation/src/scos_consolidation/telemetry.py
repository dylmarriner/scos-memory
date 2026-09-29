"""GOVERNANCE.md §32 metric emission for the Python workers.

Speaks the same wire format as packages/telemetry (a JSON array of metric
points POSTed to the observability service's /v1/metrics/ingest, whose
decoder rejects unknown fields) with the same bearer credential. Batch
workers emit a few points per cycle, so delivery is a synchronous POST
rather than the Go emitter's buffered background loop. Failures are logged
and swallowed: telemetry must never fail the work it measures.

Each worker image ships its own copy of this file; tests keep the copies
byte-identical.
"""

from __future__ import annotations

import json
import logging
import os
import ssl
import urllib.request
from datetime import datetime, timezone
from typing import Iterable, Mapping, NamedTuple

logger = logging.getLogger(__name__)

# Names mirror packages/telemetry/metrics.go.
EMBEDDING_BACKLOG = "scos.embedding.backlog"
PROMOTION_RATE = "scos.consolidation.promotion_rate"
DUPLICATE_RATE = "scos.consolidation.duplicate_rate"
CONFLICT_RATE = "scos.consolidation.conflict_rate"
POLICY_REGRESSION_RATE = "scos.learning.policy_regression_rate"

_TIMEOUT_SECONDS = 5


class Point(NamedTuple):
    name: str
    value: float
    unit: str = ""
    labels: Mapping[str, str] | None = None
    tenant: str = ""


def enabled() -> bool:
    return bool(_endpoint())


def emit(service: str, points: Iterable[Point]) -> bool:
    """Deliver points for ``service``; returns whether the collector accepted them."""
    endpoint = _endpoint()
    if not endpoint:
        return False
    timestamp = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    body = []
    for p in points:
        point = {
            "name": p.name,
            "value": float(p.value),
            "timestamp": timestamp,
            "labels": {**(p.labels or {}), "service": service},
        }
        if p.unit:
            point["unit"] = p.unit
        if p.tenant:
            point["tenant"] = str(p.tenant)
        body.append(point)
    if not body:
        return False

    request = urllib.request.Request(
        endpoint + "/v1/metrics/ingest",
        data=json.dumps(body).encode("utf-8"),
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    token = os.environ.get("SCOS_INTERNAL_SERVICE_TOKEN", "").strip()
    if token:
        request.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(request, timeout=_TIMEOUT_SECONDS, context=_tls_context(endpoint)) as response:
            return 200 <= response.status < 300
    except Exception as exc:  # telemetry must not fail the caller
        logger.warning("metric delivery to %s failed: %s", endpoint, exc)
        return False


def _endpoint() -> str:
    return os.environ.get("SCOS_OBSERVABILITY_URL", "").strip().rstrip("/")


def _tls_context(endpoint: str) -> ssl.SSLContext | None:
    if not endpoint.startswith("https://"):
        return None
    ca_file = os.environ.get("SCOS_TLS_CA_FILE", "").strip()
    return ssl.create_default_context(cafile=ca_file or None)
