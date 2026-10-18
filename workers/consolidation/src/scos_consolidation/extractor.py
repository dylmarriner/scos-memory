"""Event-driven candidate extraction from outbox jobs.

The extractor polls outbox_jobs with job_type = 'turn.completed',
reconstructs the turn context from the event log, and produces
MemoryCandidate records ready for dedup, conflict detection and
promotion.
"""

from __future__ import annotations

import json
import logging
import re
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

# A user asking outright for something to be remembered: the statement after
# the cue is the fact, worded by the user, and needs no inference.
_EXPLICIT_MEMORY = re.compile(
    r"^\s*(?:please\s+)?(?:remember|note|keep in mind|don'?t forget|for (?:future )?reference)"
    r"\b[\s,:-]*(?:that\s+)?(?P<fact>\S.*)$",
    re.IGNORECASE | re.DOTALL,
)
_TITLE_LIMIT = 80

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
        confidence = _infer_confidence(event, memory_class, session_messages)
        sensitivity = _infer_sensitivity(event, session_messages)
        if sensitivity is SensitivityClass.SECRET:
            # Credentials never enter ordinary memory (models.py refuses
            # them); the turn itself stays in the journal.
            logger.warning("event %s: turn holds credential-shaped content; not consolidated", event.get("id"))
            return []
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


def _event_data(event: dict[str, Any]) -> dict[str, Any]:
    data = event.get("data", {})
    if isinstance(data, str):
        try:
            data = json.loads(data)
        except (json.JSONDecodeError, TypeError):
            data = {}
    return data if isinstance(data, dict) else {}


# Event types whose subject is knowledge by declaration, not by guesswork.
_DECLARED_KNOWLEDGE = {
    "task.resolved": MemoryClass.PROCEDURAL,
    "decision.recorded": MemoryClass.SEMANTIC,
    "correction.applied": MemoryClass.SEMANTIC,
    "handoff.completed": MemoryClass.SEMANTIC,
    "import.completed": MemoryClass.SEMANTIC,
}

# A turn with fewer content words than this ("thanks!" / "you're welcome")
# says nothing worth remembering.
MIN_EPISODE_WORDS = 6

_CONTENT_WORD = re.compile(r"[a-z0-9][a-z0-9'._/-]*[a-z0-9]")
_FILLER = frozenset({
    "thanks", "thank", "please", "okay", "sure", "great", "welcome", "hello",
    "there", "that", "this", "with", "what", "your", "have", "will", "would",
    "could", "should", "just", "from", "they", "them", "then", "than", "been",
})


def _classify(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> MemoryClass:
    """The class of memory a turn or event yields.

    Durable knowledge comes only from what says so: a class the event
    declares (``data.memory_class``), an event type that records knowledge,
    or the user stating something to be remembered. Any other turn is an
    episodic record of the exchange, unless it carries too little to be
    worth one.
    """
    declared = _event_data(event).get("memory_class")
    if declared in {c.value for c in MemoryClass}:
        return MemoryClass(declared)
    if _explicit_statements(messages):
        return MemoryClass.SEMANTIC
    et = event.get("event_type", "")
    if et in _DECLARED_KNOWLEDGE:
        return _DECLARED_KNOWLEDGE[et]
    if _content_words(messages) < MIN_EPISODE_WORDS:
        return MemoryClass.DISCARD
    return MemoryClass.EPISODIC


def _content_words(messages: list[dict[str, Any]]) -> int:
    words = set()
    for m in messages:
        for w in _CONTENT_WORD.findall((m.get("content") or "").lower()):
            if len(w) > 3 and w not in _FILLER:
                words.add(w)
    return len(words)


def _extract_content(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
    memory_class: MemoryClass,
) -> tuple[str, str]:
    """Return (title, content) for the candidate."""
    data = _event_data(event)

    statements = _explicit_statements(messages)
    if statements:
        # The user's own words are the memory; the surrounding transcript
        # stays reachable through the evidence references.
        content = "\n".join(statements)
        return (data.get("title") or _title_from(statements[0])), content

    # The event subject is an aggregate path ("turn/<uuid>"), not a title.
    title = data.get("title", "") or data.get("subject", "")
    if not title:
        first_user = next((m.get("content", "") for m in messages if m.get("role") == "user" and m.get("content")), "")
        if first_user:
            title = _title_from(first_user)
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


def _explicit_statements(messages: list[dict[str, Any]]) -> list[str]:
    """User messages that ask for something to be remembered, cue removed."""
    out: list[str] = []
    for m in messages:
        if m.get("role") != "user":
            continue
        match = _EXPLICIT_MEMORY.match(m.get("content", "") or "")
        if not match:
            continue
        fact = " ".join(match.group("fact").split())
        if fact:
            out.append(fact[0].upper() + fact[1:])
    return out


def _title_from(text: str) -> str:
    """First sentence of ``text``, bounded, as a human-readable title."""
    line = " ".join(text.split())
    sentence = re.split(r"(?<=[.!?])\s", line, maxsplit=1)[0]
    if len(sentence) > _TITLE_LIMIT:
        sentence = sentence[: _TITLE_LIMIT - 1].rstrip() + "…"
    return sentence


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
    messages: list[dict[str, Any]],
) -> float:
    """How sure the memory is of what it states.

    A confidence the event declares is used as given. The user's own
    explicit statement is 0.9. An episodic memory is a verbatim record of
    the exchange, 0.8. Knowledge an event type records but does not score
    takes its class's default. More evidence rows do not make a statement
    truer, so they add nothing.
    """
    declared = _event_data(event).get("confidence")
    if isinstance(declared, (int, float)) and 0 <= declared <= 1:
        return float(declared)
    if memory_class == MemoryClass.SEMANTIC and _explicit_statements(messages):
        return 0.90
    return {
        MemoryClass.GOVERNING: 0.95,
        MemoryClass.REVOCATION: 0.90,
        MemoryClass.PROCEDURAL: 0.80,
        MemoryClass.SEMANTIC: 0.75,
        MemoryClass.ARTIFACT_REFERENCE: 0.80,
        MemoryClass.EPISODIC: 0.80,
        MemoryClass.WORKING: 0.60,
    }.get(memory_class, 0.60)


# Sensitivity signals, most sensitive first. Credentials make content
# secret, which ordinary consolidation refuses to store at all.
_SECRET_SIGNALS = (
    re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b"),
    re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b"),
    re.compile(r"(?i)\b(?:password|passwd|passphrase|api[ _-]?key|secret[ _-]?key|access[ _-]?token|client[ _-]?secret)\b\s*(?:is|:|=)\s*\S{4,}"),
)
_RESTRICTED_WORDS = re.compile(r"(?i)\b(?:restricted|need-to-know|clearance|\d{3}-\d{2}-\d{4})\b")
_CONFIDENTIAL_WORDS = re.compile(r"(?i)\b(?:secret|secrets|confidential|classified|credentials?)\b")


def _infer_sensitivity(
    event: dict[str, Any],
    messages: list[dict[str, Any]],
) -> SensitivityClass:
    """The most sensitive class any message signals; internal by default.

    Every message is considered and the highest class wins, so a mention of
    "open source" in one message never lowers what another reveals. Only an
    event that declares its sensitivity can make a memory public.
    """
    declared = _event_data(event).get("sensitivity")
    order = [SensitivityClass.PUBLIC, SensitivityClass.INTERNAL, SensitivityClass.CONFIDENTIAL,
             SensitivityClass.RESTRICTED, SensitivityClass.SECRET]
    level = SensitivityClass.INTERNAL
    if declared in {c.value for c in SensitivityClass}:
        level = SensitivityClass(declared)
    for m in messages:
        content = m.get("content") or ""
        if any(p.search(content) for p in _SECRET_SIGNALS):
            found = SensitivityClass.SECRET
        elif _RESTRICTED_WORDS.search(content):
            found = SensitivityClass.RESTRICTED
        elif _CONFIDENTIAL_WORDS.search(content):
            found = SensitivityClass.CONFIDENTIAL
        else:
            continue
        if order.index(found) > order.index(level):
            level = found
    return level


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
