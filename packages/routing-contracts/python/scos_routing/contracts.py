"""Python routing contracts package (§21)."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


@dataclass
class IngressEvent:
    event_id: str
    session_id: str
    tenant_id: str
    actor_id: str
    agent_id: str
    content: str
    metadata: dict[str, str] = field(default_factory=dict)
    occurred_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    def compute_event_hash(self) -> str:
        raw = f"{self.tenant_id}:{self.session_id}:{self.actor_id}:{self.content}:{self.occurred_at.isoformat()}"
        return hashlib.sha256(raw.encode("utf-8")).hexdigest()


@dataclass
class IntentClassification:
    primary_label: str
    secondary_labels: list[str] = field(default_factory=list)
    confidence: float = 1.0
    label_scores: dict[str, float] = field(default_factory=dict)
    reasoning: str = ""


@dataclass
class RouteDecision:
    decision_id: str
    ingress_event_id: str
    classification: IntentClassification
    selected_capabilities: list[str]
    requires_confirmation: bool = False
    policy_version: str = "1.3.0"
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
