"""Python context contracts package (§18)."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum


class SegmentClass(str, Enum):
    SYSTEM_MANDATORY = "system_mandatory"
    USER_CURRENT = "user_current"
    MEMORY_RETRIEVED = "memory_retrieved"
    CODE_CONTEXT = "code_context"
    TOOL_OUTPUT = "tool_output"


@dataclass
class ContextSegment:
    segment_id: str
    segment_class: SegmentClass
    content: str
    token_count: int
    priority: int = 1
    is_mandatory: bool = False


@dataclass
class ContextCapsule:
    capsule_id: str
    session_id: str
    tenant_id: str
    original_tokens: int
    compressed_tokens: int
    segments: list[ContextSegment]
    restoration_key: str
    source_objects: list["SourceObjectRef"] = field(default_factory=list)
    included_ranges: list["ContextRange"] = field(default_factory=list)
    excluded_ranges: list["ContextRange"] = field(default_factory=list)
    compression_policy_version: str = ""
    compression_policy_hash: str = ""
    token_accounting: "TokenAccounting | None" = None
    semantic_invariants: list[str] = field(default_factory=list)
    validation: "ValidationResults | None" = None
    restoration_handles: list["RestorationHandle"] = field(default_factory=list)
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class SourceObjectRef:
    object_id: str
    version: str
    content_hash: str


@dataclass
class ContextRange:
    start: int
    end: int


@dataclass
class TokenAccounting:
    method: str
    original: int
    compressed: int
    included: int
    excluded: int


@dataclass
class ValidationResults:
    valid: bool
    checks: dict[str, bool] = field(default_factory=dict)


@dataclass
class RestorationHandle:
    handle_id: str
    object_id: str
    content_hash: str


def generate_restoration_key(session_id: str, segments: list[ContextSegment]) -> str:
    raw = session_id
    for seg in segments:
        raw += f":{seg.segment_class.value}:{seg.content}"
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()[:32]
