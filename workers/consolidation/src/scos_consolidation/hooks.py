"""GOVERNANCE.md §22.6 lifecycle hook dispatch for the consolidation worker.

Python twin of packages/hook-sdk's Client.Notify: events are POSTed to
extension-manager's /v1/hooks/dispatch, which accepts only a signed claims
bearer. The worker has no caller token to forward, so each dispatch carries a
one-minute claims token minted exactly like packages/auth SignClaims (same
fixed HS256 header, same payload keys, same compact JSON) with
SCOS_SECURITY_ISSUER_SECRET, subject ``service:consolidation-worker``.

Delivery runs on a daemon thread with a short per-event timeout and failures
are logged and swallowed: a hook must never fail or stall the job it reports.
Unset SCOS_EXTENSION_MANAGER_URL disables dispatch entirely.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import logging
import os
import ssl
import threading
import time
import urllib.request
from typing import Any, Iterable, NamedTuple

logger = logging.getLogger(__name__)

SERVICE = "consolidation-worker"
ON_MEMORY_CANDIDATE = "on_memory_candidate"
ON_MEMORY_PROMOTED = "on_memory_promoted"

# packages/auth's fixed base64url of {"alg":"HS256","typ":"JWT"}.
_HEADER = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
_TIMEOUT_SECONDS = 3
_TOKEN_TTL_SECONDS = 60


class Event(NamedTuple):
    name: str
    tenant_id: str
    principal_id: str
    payload: dict[str, Any]


def _b64(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).rstrip(b"=").decode("ascii")


def _go_json(value: Any) -> str:
    # encoding/json on a map: sorted keys, no whitespace, raw UTF-8, with
    # <, >, &, U+2028 and U+2029 escaped -- byte-identical to SignClaims.
    out = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    for ch, esc in (("<", "\\u003c"), (">", "\\u003e"), ("&", "\\u0026"), (" ", "\\u2028"), (" ", "\\u2029")):
        out = out.replace(ch, esc)
    return out


def sign_claims(tenant_id: str, principal_id: str, subject: str, secret: str, expires_at: int) -> str:
    """Mint the bearer packages/auth validateClaims accepts (see SignClaims)."""
    if not secret.strip() or not tenant_id.strip() or not principal_id.strip():
        raise ValueError("signing secret, tenant_id and principal_id are required")
    payload = _go_json({
        "tenant_id": tenant_id, "principal_id": principal_id,
        "sub": subject, "agent_id": "", "delegated_authority": "",
        "exp": int(expires_at), "trace_id": "", "scopes": None, "jti": "",
    })
    body = _b64(payload.encode("utf-8"))
    signing_input = f"{_HEADER}.{body}"
    mac = hmac.new(secret.encode("utf-8"), signing_input.encode("ascii"), hashlib.sha256).digest()
    return f"{signing_input}.{_b64(mac)}"


def enabled() -> bool:
    return bool(_endpoint())


def dispatch(event: Event) -> bool:
    """Deliver one event synchronously; returns whether it was accepted."""
    endpoint = _endpoint()
    if not endpoint:
        return False
    try:
        if not event.tenant_id or not event.principal_id:
            raise ValueError("tenant and principal are required")
        secret = os.environ.get("SCOS_SECURITY_ISSUER_SECRET", "").strip()
        if not secret:
            raise ValueError("SCOS_SECURITY_ISSUER_SECRET is not configured")
        token = sign_claims(event.tenant_id, event.principal_id, "service:" + SERVICE, secret,
                            int(time.time()) + _TOKEN_TTL_SECONDS)
        request = urllib.request.Request(
            endpoint + "/v1/hooks/dispatch",
            data=json.dumps({"event": event.name, "tenant_id": event.tenant_id,
                             "principal_id": event.principal_id, "payload": event.payload}).encode("utf-8"),
            method="POST",
            headers={
                "Content-Type": "application/json",
                "Authorization": "Bearer " + token,
                "X-SCOS-Tenant-ID": event.tenant_id,
                "X-SCOS-Principal-ID": event.principal_id,
            },
        )
        with urllib.request.urlopen(request, timeout=_TIMEOUT_SECONDS, context=_tls_context(endpoint)) as response:
            if 200 <= response.status < 300:
                return True
            raise RuntimeError(f"status {response.status}")
    except Exception as exc:  # a hook must not fail the caller
        logger.warning("%s: lifecycle hook %s not delivered: %s", SERVICE, event.name, exc)
        return False


def notify(events: Iterable[Event]) -> threading.Thread | None:
    """Deliver events in order on a daemon thread; returns it (for tests)."""
    batch = list(events)
    if not batch or not enabled():
        return None
    thread = threading.Thread(target=lambda: [dispatch(e) for e in batch], name="scos-hooks", daemon=True)
    thread.start()
    return thread


def _endpoint() -> str:
    return os.environ.get("SCOS_EXTENSION_MANAGER_URL", "").strip().rstrip("/")


def _tls_context(endpoint: str) -> ssl.SSLContext | None:
    if not endpoint.startswith("https://"):
        return None
    ca_file = os.environ.get("SCOS_TLS_CA_FILE", "").strip()
    return ssl.create_default_context(cafile=ca_file or None)
