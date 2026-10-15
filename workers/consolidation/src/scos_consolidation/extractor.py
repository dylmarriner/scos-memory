"""Event-driven candidate extraction from outbox jobs.

The extractor polls outbox_jobs with job_type = 'turn.completed',
reconstructs the turn context from the event log, and produces
MemoryCandidate records ready for dedup, conflict detection and
promotion.
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timedelta, timezone
from typing import Any
from uuid import UUID, uuid4

from .models import (
    EvidenceReference,
    MemoryCandidate,
    MemoryClass,
    SensitivityClass,
)

logger = logging.getLogger(__name__)

# ── extraction triggers match §16 ──────────────────────────────────────────
EXTRACTION_TRIGGERS = frozenset({
    "turn.completed",
    "task.resolved",
    "correction.applied",
    "decision.recorded",
    "handoff.completed",
    "import.completed",
})


def extract_candidates(
    event: dict[str, Any],
    session_messages: list[dict[str, Any]],
) -> list[MemoryCandidate]:
    """Build zero or more MemoryCandidate records from one event.

    Parameters
    ----------
    event:
        The triggering event row with keys: id, tenant_id, aggregate_id,
        event_type, subject, actor_id, occurred_at, data.
    session_messages:
        All messages in the session ordered by committed_at, used for
        content extraction.

    Returns
    -------
    A (possibly empty) list of validated candidates.
    """
    event_type: str = event.get("event_type", "")
    if event_type not in EXTRACTION_TRIGGERS:
        return []

    tenant_id = _uuid(event, "tenant_id")
    event_id = _uuid(event, "id")
    actor_id = _uuid(event, "actor_id")
    occurred = _dt(event, "occurred_at")

    # ── classify the candidate class based on content heuristics ──────
    memory_class = _classify(event, session_messages)

    candidates: list[MemoryCandidate] = []

    if memory_class == MemoryClass.DISCARD:
        return []

    if memory_class == MemoryClass.JOURNAL_ONLY:
        return []  # journal is already persisted by the gateway

    # ── extract up to one semantic/procedural/governing candidate ────
    if memory_class in (
        MemoryClass.SEMANTIC,
        MemoryClass.PROCEDURAL,
        MemoryClass.GOVERNING,
        MemoryClass.REVOCATION,
        MemoryClass.EPISODIC,
        MemoryClass.WORKING,
        MemoryClass.ARTIFACT_REFERENCE,
    ):
        title, content = _extract_content(event, session_messages, memory_class)
        if not content:
            return []

        evidence = _build_evidence(event, session_messages)
        authority = _infer_authority(event, memory_class)
        confidence = _infer_confidence(event, memory_class, evidence)
        sensitivity = _infer_sensitivity(event, session_messages)
        valid_from = occurred
        valid_until = _infer_expiry(event, memory_class)

        candidate = MemoryCandidate(
            candidate_id=uuid4(),
            tenant_id=tenant_id,
            namespace_id=_resolve_namespace(event, session_messages),
            source_event_id=event_id,
            memory_class=memory_class,
            title=title,
            content=content,
            authority_level=authority,
            confidence_score=confidence,
            sensitivity_class=sensitivity,
            evidence=evidence,
            valid_from=valid_from,
            valid_until=valid_until,
            expires_at=valid_until,
            metadata={
                "extracted_from": event_type,
                "actor_id": str(actor_id) if actor_id else None,
                "extracted_at": datetime.now(timezone.utc).isoformat(),
            },
        )
        candidates.append(candidate)

    return candidates


# ── internal helpers ──────────────────────────────────────────────────────


def _classify(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> MemoryClass:
    """Heuristic classification based on event type and content signals."""
    et = event.get("event_type", "")

    # Governing signals
    if "policy" in str(event.get("data", {})).lower() or "permission" in str(event.get("data", {})).lower():
        return MemoryClass.GOVERNING
    if "revoke" in str(event.get("data", {})).lower():
        return MemoryClass.REVOCATION
    if _has_code_output(messages):
        return MemoryClass.PROCEDURAL
    if _has_factual_claim(messages):
        return MemoryClass.SEMANTIC
    if et == "task.resolved":
        return MemoryClass.PROCEDURAL
    if et == "decision.recorded":
        return MemoryClass.SEMANTIC
    if et in ("handoff.completed", "import.completed"):
        return MemoryClass.SEMANTIC
    return MemoryClass.EPISODIC  # default


def _has_code_output(messages: list[dict[str, Any]]) -> bool:
    code_signals = (
        "```", "function ", "class ", "def ", "import ", "#include",
        "package ", "SELECT ", "CREATE TABLE", "docker run",
    )
    for m in messages:
        content = m.get("content", "")
        for signal in code_signals:
            if signal in content:
                return True
    return False


def _has_factual_claim(messages: list[dict[str, Any]]) -> bool:
    fact_signals = (
        "is a ", "uses ", "runs on ", "depends on ", "located at ",
        "was built with ", "implements ", "inherits from ",
    )
    for m in messages:
        content = m.get("content", "")
        for signal in fact_signals:
            if signal in content:
                return True
    return False


def _extract_content(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
    memory_class: MemoryClass,
) -> tuple[str, str]:
    """Return (title, content) for the candidate."""
    data = event.get("data", {})
    if isinstance(data, str):
        try:
            data = json.loads(data)
        except (json.JSONDecodeError, TypeError):
            data = {}

    title = data.get("title", "") or data.get("subject", "") or event.get("subject", "")
    if not title:
        title_prefix = {
            MemoryClass.SEMANTIC: "Knowledge: ",
            MemoryClass.PROCEDURAL: "Procedure: ",
            MemoryClass.GOVERNING: "Policy: ",
            MemoryClass.REVOCATION: "Revocation: ",
            MemoryClass.EPISODIC: "Episode: ",
            MemoryClass.WORKING: "Working note: ",
            MemoryClass.ARTIFACT_REFERENCE: "Artifact: ",
        }.get(memory_class, "Note: ")
        title = title_prefix + datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

    # Content: use the last assistant message + user message context
    content_parts: list[str] = []
    for m in messages[-4:]:  # last 4 messages for context
        role = m.get("role", "unknown")
        text = m.get("content", "")
        if text:
            content_parts.append(f"[{role}]\n{text}")

    content = "\n\n".join(content_parts) if content_parts else json.dumps(data, indent=2)
    return title[:240], content


def _build_evidence(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> list[EvidenceReference]:
    """Create evidence references from the event and messages."""
    evidence: list[EvidenceReference] = []

    # The triggering event is always evidence
    event_id = str(event.get("id", ""))
    if event_id:
        evidence.append(
            EvidenceReference(
                kind="event",
                reference_id=event_id,
                content_hash=event.get("content_hash"),
            )
        )

    # Messages used for extraction become evidence
    for m in messages[-4:]:
        msg_id = str(m.get("id", ""))
        if msg_id:
            evidence.append(
                EvidenceReference(
                    kind="message",
                    reference_id=msg_id,
                    content_hash=m.get("content_hash"),
                )
            )

    return evidence


def _infer_authority(event: dict[str, Any], memory_class: MemoryClass) -> int:
    """Authority: governing = 100, procedural = 80, semantic = 70, episodic = 40."""
    base = {
        MemoryClass.GOVERNING: 100,
        MemoryClass.PROCEDURAL: 80,
        MemoryClass.SEMANTIC: 70,
        MemoryClass.REVOCATION: 90,
        MemoryClass.EPISODIC: 40,
        MemoryClass.WORKING: 30,
        MemoryClass.ARTIFACT_REFERENCE: 60,
    }.get(memory_class, 50)

    event_data = event.get("data", {})
    if isinstance(event_data, str):
        try:
            event_data = json.loads(event_data)
        except (json.JSONDecodeError, TypeError):
            event_data = {}
    official = event_data.get("authority_level", None)
    if official is not None and isinstance(official, (int, float)):
        return max(0, min(100, int(official)))
    return base


def _infer_confidence(
    event: dict[str, Any],
    memory_class: MemoryClass,
    evidence: list[EvidenceReference],
) -> float:
    """Confidence: more evidence → higher confidence."""
    base = {
        MemoryClass.GOVERNING: 0.95,
        MemoryClass.PROCEDURAL: 0.85,
        MemoryClass.SEMANTIC: 0.75,
        MemoryClass.REVOCATION: 0.90,
        MemoryClass.EPISODIC: 0.50,
        MemoryClass.WORKING: 0.40,
        MemoryClass.ARTIFACT_REFERENCE: 0.80,
    }.get(memory_class, 0.60)

    evidence_boost = min(len(evidence) * 0.05, 0.20)
    return min(base + evidence_boost, 1.0)


def _infer_sensitivity(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> SensitivityClass:
    """Default 'internal', escalate if signals present."""
    for m in messages:
        content = m.get("content", "")
        if any(w in content.lower() for w in ("secret", "confidential", "classified")):
            return SensitivityClass.CONFIDENTIAL
        if any(w in content.lower() for w in ("restricted", "need-to-know", "clearance")):
            return SensitivityClass.RESTRICTED
        if any(w in content.lower() for w in ("public", "open-source", "open source")):
            return SensitivityClass.PUBLIC
    return SensitivityClass.INTERNAL


class UnresolvedNamespaceError(ValueError):
    """A candidate's namespace could not be determined, so it cannot be stored."""


def _resolve_namespace(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> UUID:
    """The namespace the memory belongs to.

    The worker attaches the turn's session namespace as ``namespace_id`` on
    the event; an explicit namespace in the event data takes precedence. A
    memory with no resolvable namespace is an error, never an invented ID.
    """
    del messages
    data = event.get("data", {})
    if isinstance(data, str):
        try:
            data = json.loads(data)
        except (json.JSONDecodeError, TypeError):
            data = {}
    for ns in (data.get("namespace_id"), data.get("namespace"), event.get("namespace_id")):
        if not ns:
            continue
        if isinstance(ns, UUID):
            return ns
        try:
            return UUID(str(ns))
        except (ValueError, AttributeError):
            continue
    raise UnresolvedNamespaceError(f"event {event.get('id')} has no resolvable namespace")


def _infer_expiry(
    event: dict[str, Any],
    memory_class: MemoryClass,
) -> datetime | None:
    """Expiry: working notes expire 7 days after the event, episodic memories
    30 days after it; durable classes are indefinite."""
    occurred = event.get("occurred_at")
    if isinstance(occurred, str):
        try:
            occurred = datetime.fromisoformat(occurred.replace("Z", "+00:00"))
        except ValueError:
            occurred = None
    if not isinstance(occurred, datetime):
        occurred = datetime.now(timezone.utc)
    if occurred.tzinfo is None:
        occurred = occurred.replace(tzinfo=timezone.utc)
    occurred = occurred.replace(microsecond=0)
    if memory_class == MemoryClass.WORKING:
        return occurred + timedelta(days=7)
    if memory_class == MemoryClass.EPISODIC:
        return occurred + timedelta(days=30)
    return None  # indefinite for durable classes


def _uuid(obj: dict[str, Any], key: str) -> UUID | None:
    val = obj.get(key)
    if val is None:
        return None
    if isinstance(val, UUID):
        return val
    try:
        return UUID(str(val))
    except (ValueError, AttributeError):
        return None


def _dt(obj: dict[str, Any], key: str) -> datetime | None:
    val = obj.get(key)
    if val is None:
        return None
    if isinstance(val, datetime):
        return val
    try:
        return datetime.fromisoformat(str(val).replace("Z", "+00:00"))
    except (ValueError, TypeError):
        return None
