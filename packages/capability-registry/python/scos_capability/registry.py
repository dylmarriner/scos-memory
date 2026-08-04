"""Python capability registry package (§21.6)."""

from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Optional


@dataclass
class CapabilityRecord:
    capability_id: str
    name: str
    category: str
    description: str
    required_permissions: list[str] = field(default_factory=list)
    status: str = "healthy"
    confidence_threshold: float = 0.75
    registered_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


class CapabilityRegistry:
    def __init__(self):
        self._capabilities: dict[str, CapabilityRecord] = {}

    def register(self, record: CapabilityRecord) -> None:
        self._capabilities[record.capability_id] = record

    def get(self, capability_id: str) -> Optional[CapabilityRecord]:
        return self._capabilities.get(capability_id)

    def list_healthy(self, category: Optional[str] = None) -> list[CapabilityRecord]:
        return [
            cap for cap in self._capabilities.values()
            if cap.status == "healthy" and (category is None or cap.category == category)
        ]
