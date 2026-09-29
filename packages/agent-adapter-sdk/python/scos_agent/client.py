"""Python agent adapter SDK package (§23.4): a client for the
compatibility-gateway's context-envelope API."""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field, fields
from typing import Optional


@dataclass
class ContextEnvelope:
    """Stored context envelope for a session (GET /v1/envelope)."""

    agent_id: str
    session_id: str
    tenant_id: str = ""
    principal_id: str = ""
    governing_constraints: str = ""
    user_model: str = ""
    facts: list[str] = field(default_factory=list)
    learnings: list[str] = field(default_factory=list)
    task_state: str = ""
    episodic_history: list[str] = field(default_factory=list)
    contradictions: list[str] = field(default_factory=list)
    code_references: list[str] = field(default_factory=list)
    artifacts: list[str] = field(default_factory=list)
    sources: list[str] = field(default_factory=list)
    restoration_references: list[str] = field(default_factory=list)
    format: str = ""

    @classmethod
    def from_json(cls, data: dict) -> "ContextEnvelope":
        known = {f.name for f in fields(cls)}
        return cls(**{k: v for k, v in data.items() if k in known and v is not None})


@dataclass
class RenderedEnvelope:
    """Envelope rendered for prompt injection (POST /v1/envelope/render)."""

    format: str
    content: str


class EnvelopeNotFoundError(LookupError):
    """No envelope is stored for the requested session."""


class GatewayError(RuntimeError):
    """The compatibility gateway rejected the request or was unreachable."""


RENDER_FORMATS = ("markdown", "text", "json")


class AgentAdapterClient:
    def __init__(
        self,
        agent_id: str,
        gateway_url: str,
        *,
        bearer_token: str = "",
        tenant_id: str = "",
        principal_id: str = "",
        timeout: float = 5.0,
    ):
        if not agent_id:
            raise ValueError("agent_id is required")
        if not gateway_url:
            raise ValueError("gateway_url is required")
        if not bearer_token and not (tenant_id and principal_id):
            raise ValueError("a bearer token or both tenant_id and principal_id are required")
        self.agent_id = agent_id
        self.gateway_url = gateway_url.rstrip("/")
        self._bearer_token = bearer_token
        self._tenant_id = tenant_id
        self._principal_id = principal_id
        self._timeout = timeout

    def get_envelope(self, session_id: str) -> ContextEnvelope:
        if not session_id:
            raise ValueError("session_id is required")
        query = urllib.parse.urlencode({"session_id": session_id})
        return ContextEnvelope.from_json(self._request("GET", "/v1/envelope?" + query))

    def render_envelope(self, session_id: str, target_format: str = "markdown") -> RenderedEnvelope:
        if not session_id:
            raise ValueError("session_id is required")
        if target_format not in RENDER_FORMATS:
            raise ValueError(f"unsupported render format {target_format!r}")
        data = self._request("POST", "/v1/envelope/render", {
            "session_id": session_id,
            "target_format": target_format,
            "agent_id": self.agent_id,
        })
        return RenderedEnvelope(format=data.get("format", target_format), content=data["content"])

    def _request(self, method: str, path: str, body: Optional[dict] = None) -> dict:
        headers = {}
        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if self._bearer_token:
            headers["Authorization"] = "Bearer " + self._bearer_token
        if self._tenant_id:
            headers["X-SCOS-Tenant-ID"] = self._tenant_id
        if self._principal_id:
            headers["X-SCOS-Principal-ID"] = self._principal_id
        req = urllib.request.Request(self.gateway_url + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as resp:
                return json.loads(resp.read().decode("utf-8"))
        except urllib.error.HTTPError as error:
            if error.code == 404:
                raise EnvelopeNotFoundError(path) from error
            detail = error.read().decode("utf-8", "replace")[:500]
            raise GatewayError(f"compatibility gateway returned HTTP {error.code}: {detail}") from error
        except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
            raise GatewayError(f"compatibility gateway {method} {path} failed: {error}") from error
