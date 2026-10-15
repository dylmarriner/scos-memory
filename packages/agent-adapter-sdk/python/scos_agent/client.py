"""Python agent adapter SDK package (§23.4)."""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field


@dataclass
class ContextEnvelope:
    """Portable context envelope (§23.5) as served by GET /v1/envelope."""

    agent_id: str
    session_id: str
    format: str
    tenant_id: str = ""
    principal_id: str = ""
    governing_constraints: str = ""
    user_model: str = ""
    task_state: str = ""
    facts: list[str] = field(default_factory=list)
    learnings: list[str] = field(default_factory=list)
    episodic_history: list[str] = field(default_factory=list)
    contradictions: list[str] = field(default_factory=list)
    code_references: list[str] = field(default_factory=list)
    artifacts: list[str] = field(default_factory=list)
    sources: list[str] = field(default_factory=list)
    restoration_references: list[str] = field(default_factory=list)


class EnvelopeError(RuntimeError):
    """Raised when the gateway did not return the requested envelope."""

    def __init__(self, message: str, status: int | None = None):
        super().__init__(message)
        self.status = status


class EnvelopeNotFound(EnvelopeError):
    """The gateway holds no envelope for the session under the caller's tenant."""


_STRING_FIELDS = ("tenant_id", "principal_id", "governing_constraints", "user_model", "task_state", "format")
_LIST_FIELDS = (
    "facts", "learnings", "episodic_history", "contradictions",
    "code_references", "artifacts", "sources", "restoration_references",
)


def _parse_envelope(data: object, session_id: str, agent_id: str) -> ContextEnvelope:
    if not isinstance(data, dict):
        raise EnvelopeError("gateway response is not a JSON object")
    for name, expected in (("session_id", session_id), ("agent_id", agent_id)):
        if data.get(name) != expected:
            raise EnvelopeError(f"gateway envelope {name} {data.get(name)!r} does not match the requested {expected!r}")
    values: dict = {"agent_id": agent_id, "session_id": session_id}
    for name in _STRING_FIELDS:
        value = data.get(name) or ""
        if not isinstance(value, str):
            raise EnvelopeError(f"gateway envelope field {name!r} is not a string")
        values[name] = value
    for name in _LIST_FIELDS:
        value = data.get(name) or []
        if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
            raise EnvelopeError(f"gateway envelope field {name!r} is not a list of strings")
        values[name] = list(value)
    return ContextEnvelope(**values)


class AgentAdapterClient:
    def __init__(self, agent_id: str, gateway_url: str, bearer_token: str, timeout: float = 5.0):
        if not agent_id:
            raise ValueError("agent_id is required")
        if not gateway_url:
            raise ValueError("gateway_url is required")
        if not bearer_token:
            raise ValueError("bearer_token is required")
        self.agent_id = agent_id
        self.gateway_url = gateway_url.rstrip("/")
        self.bearer_token = bearer_token
        self.timeout = timeout

    def fetch_envelope(self, session_id: str) -> ContextEnvelope:
        """Read the stored context envelope for ``session_id``.

        The gateway resolves tenant and principal from the bearer token. Any
        failure raises ``EnvelopeError`` (``EnvelopeNotFound`` for 404).
        """
        if not session_id:
            raise ValueError("session_id is required")
        req = urllib.request.Request(
            f"{self.gateway_url}/v1/envelope?session_id={urllib.parse.quote(session_id, safe='')}",
            headers={"Authorization": f"Bearer {self.bearer_token}", "Accept": "application/json"},
            method="GET",
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                body = resp.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read(4096).decode("utf-8", "replace").strip()
            cls = EnvelopeNotFound if exc.code == 404 else EnvelopeError
            raise cls(f"gateway returned {exc.code}: {detail}", status=exc.code) from exc
        except (urllib.error.URLError, OSError) as exc:
            raise EnvelopeError(f"fetch envelope: {exc}") from exc
        try:
            data = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise EnvelopeError(f"failed to decode gateway response: {exc}") from exc
        return _parse_envelope(data, session_id, self.agent_id)
