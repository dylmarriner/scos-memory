from __future__ import annotations

from datetime import datetime
from enum import StrEnum
from typing import Any
from uuid import UUID

from pydantic import BaseModel, Field, model_validator


class MemoryClass(StrEnum):
    DISCARD = "discard"
    JOURNAL_ONLY = "journal_only"
    WORKING = "working"
    EPISODIC = "episodic"
    SEMANTIC = "semantic"
    PROCEDURAL = "procedural"
    GOVERNING = "governing"
    ARTIFACT_REFERENCE = "artifact_reference"
    REVOCATION = "revocation"


class SensitivityClass(StrEnum):
    PUBLIC = "public"
    INTERNAL = "internal"
    CONFIDENTIAL = "confidential"
    RESTRICTED = "restricted"
    SECRET = "secret"


class EvidenceReference(BaseModel):
    kind: str = Field(min_length=1, max_length=64)
    reference_id: str = Field(min_length=1, max_length=512)
    content_hash: str | None = Field(default=None, max_length=128)


class MemoryCandidate(BaseModel):
    candidate_id: UUID
    tenant_id: UUID
    namespace_id: UUID
    source_event_id: UUID
    memory_class: MemoryClass
    title: str = Field(min_length=1, max_length=240)
    content: str = Field(min_length=1)
    authority_level: int = Field(ge=0, le=100)
    confidence_score: float = Field(ge=0, le=1)
    sensitivity_class: SensitivityClass
    evidence: list[EvidenceReference] = Field(default_factory=list)
    valid_from: datetime | None = None
    valid_until: datetime | None = None
    expires_at: datetime | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)

    @model_validator(mode="after")
    def enforce_governance(self) -> "MemoryCandidate":
        if self.sensitivity_class is SensitivityClass.SECRET:
            raise ValueError("secret content must not enter ordinary memory consolidation")
        if self.memory_class in {
            MemoryClass.SEMANTIC,
            MemoryClass.PROCEDURAL,
            MemoryClass.GOVERNING,
            MemoryClass.REVOCATION,
        } and not self.evidence:
            raise ValueError("durable knowledge candidates require evidence")
        if self.valid_from and self.valid_until and self.valid_until <= self.valid_from:
            raise ValueError("valid_until must be later than valid_from")
        return self
