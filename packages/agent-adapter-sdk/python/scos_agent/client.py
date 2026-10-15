"""Python agent adapter SDK package (§23.4)."""

from __future__ import annotations

import json
from dataclasses import dataclass, field, fields
from datetime import datetime, timezone
import urllib.error
import urllib.request


@dataclass
class PortableEnvelope:
    envelope_id: str
    session_id: str
    tenant_id: str
    agent_id: str
    context_items: list[str] = field(default_factory=list)
    permissions: list[str] = field(default_factory=list)
    metadata: dict[str, str] = field(default_factory=dict)
    generated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class EnvelopeRenderError(RuntimeError):
    """Raised when the gateway did not return a governed envelope.

    An envelope carries the permissions an agent may exercise, so it is only
    ever taken from the gateway. When the gateway cannot be reached, refuses
    the request or answers with something that is not an envelope, the caller
    gets this error and no permissions at all.
    """

    def __init__(self, message: str, status: int | None = None):
        super().__init__(message)
        self.status = status


_REQUIRED_FIELDS = ("envelope_id", "session_id", "tenant_id", "agent_id")
_KNOWN_FIELDS = frozenset(f.name for f in fields(PortableEnvelope))


def _string_list(data: dict, name: str) -> list[str]:
    value = data.get(name)
    if value is None:
        return []
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        raise EnvelopeRenderError(f"gateway envelope field {name!r} is not a list of strings")
    return list(value)


def _parse_envelope(data: object, session_id: str, tenant_id: str, agent_id: str) -> PortableEnvelope:
    if not isinstance(data, dict):
        raise EnvelopeRenderError("gateway response is not a JSON object")
    missing = [name for name in _REQUIRED_FIELDS if not isinstance(data.get(name), str) or not data.get(name)]
    if missing:
        raise EnvelopeRenderError(f"gateway response is not an envelope: missing {', '.join(missing)}")
    # The gateway must answer for the identity that was asked about; an
    # envelope for another session, tenant or agent is never accepted.
    for name, expected in (("session_id", session_id), ("tenant_id", tenant_id), ("agent_id", agent_id)):
        if data[name] != expected:
            raise EnvelopeRenderError(
                f"gateway envelope {name} {data[name]!r} does not match the requested {expected!r}"
            )

    metadata = data.get("metadata") or {}
    if not isinstance(metadata, dict) or not all(
        isinstance(k, str) and isinstance(v, str) for k, v in metadata.items()
    ):
        raise EnvelopeRenderError("gateway envelope field 'metadata' is not a string map")

    raw_generated = data.get("generated_at")
    if not isinstance(raw_generated, str) or not raw_generated:
        raise EnvelopeRenderError("gateway envelope has no generated_at timestamp")
    try:
        generated_at = datetime.fromisoformat(raw_generated.replace("Z", "+00:00"))
    except ValueError as exc:
        raise EnvelopeRenderError(f"gateway envelope generated_at is not a timestamp: {raw_generated!r}") from exc

    unknown = sorted(set(data) - _KNOWN_FIELDS)
    if unknown:
        raise EnvelopeRenderError(f"gateway envelope has unexpected fields: {', '.join(unknown)}")

    return PortableEnvelope(
        envelope_id=data["envelope_id"],
        session_id=data["session_id"],
        tenant_id=data["tenant_id"],
        agent_id=data["agent_id"],
        context_items=_string_list(data, "context_items"),
        permissions=_string_list(data, "permissions"),
        metadata=dict(metadata),
        generated_at=generated_at,
    )


class AgentAdapterClient:
    def __init__(self, agent_id: str, gateway_url: str = "http://127.0.0.1:8080", timeout: float = 3.0):
        if not agent_id:
            raise ValueError("agent_id is required")
        self.agent_id = agent_id
        self.gateway_url = gateway_url.rstrip("/")
        self.timeout = timeout

    def render_envelope(self, session_id: str, tenant_id: str) -> PortableEnvelope:
        """Ask the gateway to compile the governed envelope for a session.

        Raises ``EnvelopeRenderError`` on any failure; there is no local
        fallback, because a fabricated envelope would grant permissions the
        gateway never issued.
        """
        payload = json.dumps({
            "agent_id": self.agent_id,
            "session_id": session_id,
            "tenant_id": tenant_id,
        }).encode("utf-8")

        req = urllib.request.Request(
            f"{self.gateway_url}/v1/envelope/render",
            data=payload,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                body = resp.read()
        except urllib.error.HTTPError as exc:
            raise EnvelopeRenderError(f"gateway returned {exc.code} {exc.reason}", status=exc.code) from exc
        except (urllib.error.URLError, OSError) as exc:
            raise EnvelopeRenderError(f"render governed envelope: {exc}") from exc

        try:
            data = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise EnvelopeRenderError(f"failed to decode gateway response: {exc}") from exc
        return _parse_envelope(data, session_id, tenant_id, self.agent_id)
