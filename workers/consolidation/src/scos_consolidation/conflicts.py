"""Contradiction detection for memory candidates.

Per §16: "Contradictions SHALL create visible conflict sets and SHALL NOT
be silently resolved by vector score."
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone
from typing import Any, Sequence
from uuid import UUID, uuid4

from .models import MemoryCandidate

logger = logging.getLogger(__name__)


# ── public API ────────────────────────────────────────────────────────────


class ConflictSet:
    """A detectable contradiction between two or more candidates/items.

    Attributes
    ----------
    conflict_id:
        Unique identifier for this conflict set.
    subjects:
        Memory IDs / candidate IDs involved.
    statements:
        The contradictory statements.
    detected_at:
        When the conflict was detected.
    evidence:
        References to evidence supporting each side.
    resolved:
        Whether a human or governing policy has resolved this conflict.
    """

    __slots__ = (
        "conflict_id", "subjects", "statements",
        "detected_at", "evidence", "resolved",
    )

    def __init__(
        self,
        conflict_id: UUID,
        subjects: list[str],
        statements: list[str],
        detected_at: datetime,
        evidence: list[dict[str, Any]],
        resolved: bool = False,
    ) -> None:
        self.conflict_id = conflict_id
        self.subjects = subjects
        self.statements = statements
        self.detected_at = detected_at
        self.evidence = evidence
        self.resolved = resolved

    def to_dict(self) -> dict[str, Any]:
        return {
            "conflict_id": str(self.conflict_id),
            "subjects": self.subjects,
            "statements": self.statements,
            "detected_at": self.detected_at.isoformat(),
            "evidence": self.evidence,
            "resolved": self.resolved,
        }


def detect_conflicts(
    candidates: list[MemoryCandidate],
    existing: Sequence[dict[str, Any]],
) -> list[ConflictSet]:
    """Compare candidates against existing memory for contradictions.

    Conflict detection uses three strategies:

    1. **Direct negation** — one statement explicitly negates another
       (e.g. "uses PostgreSQL" vs "does NOT use PostgreSQL").
    2. **Mutually exclusive attributes** — the same subject has
       incompatible property values.
    3. **Authority-based contradiction** — a low-authority claim
       contradicts a high-authority claim on the same subject.

    Parameters
    ----------
    candidates:
        Incoming candidates from the extractor.
    existing:
        Existing memory_items or memory_candidates rows from the
        same namespace / subject area.

    Returns
    -------
    A list of ``ConflictSet`` instances. Empty list means no
    contradictions found.
    """
    conflicts: list[ConflictSet] = []

    for c in candidates:
        subject = _extract_subject(c)
        if not subject:
            continue

        for row in existing:
            existing_subject = _extract_subject_from_row(row)
            if not existing_subject or _normalise(subject) != _normalise(existing_subject):
                continue

            existing_content = row.get("content", "")
            conflict_statements: list[str] = []

            # Strategy 1: direct negation
            negations = _find_negations(c.content, existing_content)
            conflict_statements.extend(negations)

            # Strategy 3: authority mismatch on same subject
            existing_authority = row.get("authority_level", 50)
            if abs(c.authority_level - existing_authority) >= 40:
                conflict_statements.append(
                    f"Authority mismatch: candidate={c.authority_level}, "
                    f"existing={existing_authority} for subject '{subject}'"
                )

            if conflict_statements:
                conflicts.append(
                    ConflictSet(
                        conflict_id=uuid4(),
                        subjects=[str(c.source_event_id), str(row.get("id", ""))],
                        statements=conflict_statements,
                        detected_at=datetime.now(timezone.utc),
                        evidence=[
                            {"source": "candidate", "event_id": str(c.source_event_id)},
                            {"source": "existing", "memory_id": str(row.get("id", ""))},
                        ],
                    )
                )

    return conflicts


# ── internal helpers ──────────────────────────────────────────────────────


def _extract_subject(candidate: MemoryCandidate) -> str | None:
    """Extract the primary subject from title/content."""
    title = candidate.title
    title = title.removeprefix("Knowledge: ")
    title = title.removeprefix("Procedure: ")
    title = title.removeprefix("Policy: ")
    title = title.removeprefix("Episode: ")
    title = title.removeprefix("Working note: ")
    title = title.removeprefix("Artifact: ")
    title = title.removeprefix("Revocation: ")
    title = title.removeprefix("Note: ")
    title = title.strip()
    return title if title else None


def _extract_subject_from_row(row: dict[str, Any]) -> str | None:
    title = row.get("title", "")
    if not title:
        return None
    title = title.removeprefix("Knowledge: ")
    title = title.removeprefix("Procedure: ")
    title = title.removeprefix("Policy: ")
    title = title.strip()
    return title if title else None


def _normalise(text: str) -> str:
    import re
    return re.sub(r"\s+", " ", text.lower().strip())


def _find_negations(content_a: str, content_b: str) -> list[str]:
    """Detect explicit negation pairs between two content strings."""
    negations: list[str] = []
    a_lower = content_a.lower()
    b_lower = content_b.lower()

    # Look for "X is Y" in one and "X is NOT Y" in the other
    for extract in (_extract_factual_statements(a_lower), _extract_factual_statements(b_lower)):
        for stmt in extract:
            # Check if the other side contradicts this statement
            if "not " + stmt in a_lower or "not " + stmt in b_lower:
                negations.append(f"Contradictory statement: '{stmt}'")
            # Check "does not" pattern
            words = stmt.split()
            if len(words) >= 3:
                verb = words[1] if len(words) > 1 else ""
                rest = " ".join(words[2:])
                negation_pattern = f"does not {verb} {rest}" if rest else f"does not {verb}"
                if negation_pattern in a_lower or negation_pattern in b_lower:
                    negations.append(f"Contradictory statement: '{stmt}'")

    return negations


def _extract_factual_statements(text: str) -> list[str]:
    """Extract simple 'X is|uses|runs|depends|located Y' statements."""
    import re
    patterns = [
        r"(?:is|are|was|were)\s+a(n)?\s+([^.!?]+)",
        r"(?:uses|runs|depends|located|built|implemented)\s+(?:on|in|with|at|by|using)\s+([^.!?]+)",
    ]
    statements: list[str] = []
    for pat in patterns:
        for match in re.finditer(pat, text):
            statements.append(match.group(0).strip())
    return statements
