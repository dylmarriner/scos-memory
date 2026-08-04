"""Python agent adapter SDK package (§23.4)."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
import urllib.request


@dataclass
class PortableEnvelope:
    envelope_id: str
    session_id: str
    tenant_id: str
    agent_id: str
    context_items: list[str] = field(default_factory=list)
    permissions: list[str] = field(default_factory=list)
    generated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class AgentAdapterClient:
    def __init__(self, agent_id: str, gateway_url: str = "http://127.0.0.1:8080"):
        self.agent_id = agent_id
        self.gateway_url = gateway_url.rstrip("/")

    def render_envelope(self, session_id: str, tenant_id: str) -> PortableEnvelope:
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
            with urllib.request.urlopen(req, timeout=3) as resp:
                data = json.loads(resp.read().decode("utf-8"))
                return PortableEnvelope(**data)
        except Exception:
            # Deterministic fallback envelope when server is offline
            return PortableEnvelope(
                envelope_id=f"env-py-{session_id}",
                session_id=session_id,
                tenant_id=tenant_id,
                agent_id=self.agent_id,
                permissions=["read", "execute"],
            )
