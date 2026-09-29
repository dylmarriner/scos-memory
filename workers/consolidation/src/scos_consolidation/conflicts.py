"""Contradiction detection for memory candidates.

Per §16: "Contradictions SHALL create visible conflict sets and SHALL NOT
be silently resolved by vector score."
"""

from __future__ import annotations

import logging
import re
from datetime import datetime, timezone
from typing import Any, NamedTuple, Sequence
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
    candidates: Sequence[MemoryCandidate],
    existing: Sequence[dict[str, Any]],
) -> list[ConflictSet]:
    """Compare candidates against existing memory for contradictions.

    Conflict detection uses three strategies:

    1. **Direct negation** — one statement explicitly negates another
       (e.g. "uses PostgreSQL" vs "does not use PostgreSQL").
    2. **Mutually exclusive attributes** — the same subject has
       incompatible values for a single-valued property (e.g. "runs on
       db01" vs "runs on db02").
    3. **Authority-based contradiction** — a low-authority claim
       contradicts a high-authority claim on the same subject.

    Statements are parsed into (subject, predicate, object, negated) claims,
    so subjects are matched from what was said as well as from titles --
    free-text turns carry no meaningful title of their own.

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
        title_subject = _extract_subject(c)
        c_claims = extract_claims(c.content)
        c_subjects = {claim.subject for claim in c_claims}

        for row in existing:
            existing_content = row.get("content") or ""
            row_claims = extract_claims(existing_content)
            row_title_subject = _extract_subject_from_row(row)

            same_title = bool(title_subject and row_title_subject) and (
                _normalise(title_subject) == _normalise(row_title_subject)
            )
            shared_subjects = c_subjects & {claim.subject for claim in row_claims}
            if not same_title and not shared_subjects:
                continue

            conflict_statements: list[str] = []

            # Strategies 1 and 2: contradictory claims about a shared subject
            conflict_statements.extend(_contradictions(c_claims, row_claims))

            # Strategy 3: authority mismatch on same subject
            existing_authority = row.get("authority_level", 50)
            if abs(c.authority_level - existing_authority) >= 40:
                subject = title_subject if same_title else sorted(shared_subjects)[0]
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


# ── claim extraction ──────────────────────────────────────────────────────


class Claim(NamedTuple):
    subject: str
    predicate: str
    obj: str
    negated: bool

    def render(self) -> str:
        verb = {"uses": "does not use", "runs on": "does not run on",
                "depends on": "does not depend on"}.get(self.predicate, "is not " + self.predicate)
        if self.predicate == "is":
            verb = "is not"
        return f"{self.subject} {verb if self.negated else self.predicate} {self.obj}"


# Predicates that hold exactly one value at a time for a given subject: two
# different positive values are a contradiction. "uses"/"depends on"/"is"
# are deliberately absent -- a service can use many things.
_SINGLE_VALUED = frozenset({"runs on", "located at", "hosted on", "deployed to"})

# Active verbs: third-person/past form → canonical predicate, and the base
# form used after a negating auxiliary ("does not use").
_ACTIVE = {
    "uses": "uses", "used": "uses",
    "runs on": "runs on", "ran on": "runs on",
    "depends on": "depends on", "depended on": "depends on",
}
_ACTIVE_BASE = {"use": "uses", "run on": "runs on", "depend on": "depends on"}
# Participles that follow a form of "to be" ("is hosted on").
_PASSIVE = {
    "located at": "located at", "located in": "located at",
    "hosted on": "hosted on", "hosted in": "hosted on", "hosted at": "hosted on",
    "deployed to": "deployed to", "deployed on": "deployed to", "deployed in": "deployed to",
}

_NEG_AUX = r"(?:does not|doesn't|do not|don't|did not|didn't|no longer|never)"
_NEG_ADV = r"(?:no longer|never|not)"
_BE = r"(?:is|are|was|were)"
_BE_NEG = r"(?:is not|isn't|are not|aren't|was not|wasn't|were not|weren't|is no longer|are no longer)"
_SUBJ = r"(?P<subj>[a-z0-9][\w./-]*(?:\s+[\w./-]+){0,5}?)"
_OBJ = r"(?P<obj>.+)"


def _alt(words: Sequence[str], name: str = "verb") -> str:
    """Named alternation, longest first so "runs on" wins over a shorter prefix."""
    return f"(?P<{name}>" + "|".join(sorted((re.escape(w) for w in words), key=len, reverse=True)) + ")"


# Ordered: negated forms first, so "is not X" is never read as "is (not X)".
_CLAUSE_PATTERNS: list[tuple[re.Pattern[str], dict[str, str] | None, bool]] = [
    (re.compile(rf"^{_SUBJ}\s+{_NEG_AUX}\s+{_alt(_ACTIVE_BASE)}\s+{_OBJ}$"), _ACTIVE_BASE, True),
    (re.compile(rf"^{_SUBJ}\s+{_NEG_ADV}\s+{_alt(_ACTIVE)}\s+{_OBJ}$"), _ACTIVE, True),
    (re.compile(rf"^{_SUBJ}\s+{_BE_NEG}\s+{_alt(_PASSIVE)}\s+{_OBJ}$"), _PASSIVE, True),
    (re.compile(rf"^{_SUBJ}\s+{_BE_NEG}\s+{_OBJ}$"), None, True),
    (re.compile(rf"^{_SUBJ}\s+{_alt(_ACTIVE)}\s+{_OBJ}$"), _ACTIVE, False),
    (re.compile(rf"^{_SUBJ}\s+{_BE}\s+{_alt(_PASSIVE)}\s+{_OBJ}$"), _PASSIVE, False),
    (re.compile(rf"^{_SUBJ}\s+{_BE}\s+{_OBJ}$"), None, False),
]
# A clause after "and"/"but" that omits its subject ("... and runs on db01").
_ELIDED = re.compile(
    rf"^(?:(?P<neg>{_NEG_AUX})\s+{_alt(_ACTIVE_BASE, 'base')}|{_alt(_ACTIVE)})\s+{_OBJ}$"
)

_PRONOUNS = frozenset({"it", "this", "that", "they", "he", "she", "we", "i", "you", "there", "which", "who", "what"})
_ARTICLES = re.compile(r"^(?:the|a|an|our|my|your|their|its|this|that)\s+")
_SENTENCE_SPLIT = re.compile(r"[.!?](?:\s+|$)|\n+")
_CLAUSE_SPLIT = re.compile(r"\s*(?:;|,|:\s)\s*|\s+(?:and|but)\s+")
_ROLE_TAG = re.compile(r"^\[[a-z_]+\]\s*")


def extract_claims(text: str) -> list[Claim]:
    """Parse simple declarative statements into normalised claims."""
    claims: list[Claim] = []
    for sentence in _SENTENCE_SPLIT.split(text.lower()):
        sentence = _ROLE_TAG.sub("", " ".join(sentence.split()))
        last_subject: str | None = None
        for clause in _CLAUSE_SPLIT.split(sentence):
            clause = clause.strip()
            if not clause:
                continue
            claim = _parse_clause(clause, last_subject)
            if claim is not None:
                claims.append(claim)
                last_subject = claim.subject
    return claims


def _parse_clause(clause: str, last_subject: str | None) -> Claim | None:
    for pattern, verbs, negated in _CLAUSE_PATTERNS:
        m = pattern.match(clause)
        if not m:
            continue
        subject = _clean_term(m.group("subj"))
        if not subject or subject in _PRONOUNS:
            return None
        predicate = verbs[m.group("verb")] if verbs is not None else "is"
        obj = _clean_term(m.group("obj"))
        return Claim(subject, predicate, obj, negated) if obj else None
    if last_subject:
        m = _ELIDED.match(clause)
        if m:
            negated = m.group("neg") is not None
            verb = m.group("base") if negated else m.group("verb")
            predicate = (_ACTIVE_BASE if negated else _ACTIVE)[verb]
            obj = _clean_term(m.group("obj"))
            return Claim(last_subject, predicate, obj, negated) if obj else None
    return None


def _clean_term(term: str) -> str:
    term = _ARTICLES.sub("", term.strip().strip("\"'`()[]"))
    return " ".join(term.split()).rstrip(".,;:!?")


def _contradictions(ours: Sequence[Claim], theirs: Sequence[Claim]) -> list[str]:
    found: list[str] = []
    for a in ours:
        for b in theirs:
            if a.subject != b.subject or a.predicate != b.predicate:
                continue
            if a.obj == b.obj and a.negated != b.negated:
                found.append(f"Contradictory statement: '{a.render()}' vs '{b.render()}'")
            elif (a.predicate in _SINGLE_VALUED and not a.negated and not b.negated
                  and a.obj != b.obj):
                found.append(f"Mutually exclusive: '{a.render()}' vs '{b.render()}'")
    return list(dict.fromkeys(found))


# ── subject helpers ───────────────────────────────────────────────────────


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
    return re.sub(r"\s+", " ", text.lower().strip())
