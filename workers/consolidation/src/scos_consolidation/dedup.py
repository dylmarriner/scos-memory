"""Duplicate detection and evidence merging for memory candidates.

Per §16: "Duplicates SHALL merge evidence rather than produce endless
paraphrases."
"""

from __future__ import annotations

import logging
from hashlib import sha256
from typing import Sequence

from .models import EvidenceReference, MemoryCandidate
from .statements import duplicate_score, restates

logger = logging.getLogger(__name__)


# ── public API ────────────────────────────────────────────────────────────


def deduplicate(
    candidates: list[MemoryCandidate],
    existing: Sequence[dict],
) -> list[DedupResult]:
    """Classify each candidate as *new*, *merged* or *supersedes*.

    *merged*: an existing memory (or pending candidate) already states this;
    the candidate's evidence belongs on that record and nothing new is
    stored. *supersedes*: the candidate restates an existing memory with
    significantly higher authority, so it replaces that memory.

    Parameters
    ----------
    candidates:
        Incoming candidate records from the extractor.
    existing:
        Rows from ``memory_candidates`` or ``memory_items`` that share
        the same tenant_id, namespace_id and a similar title/content
        fingerprint.

    Returns
    -------
    A list of ``DedupResult`` for the caller to act on.
    """
    results: list[DedupResult] = []
    for c in candidates:
        fp = _fingerprint(c)
        match = _find_match(fp, existing, c.content)
        if match is None:
            results.append(DedupResult(candidate=c, action="new"))
            continue

        if _outranks(c, match):
            results.append(DedupResult(candidate=c, action="supersedes", supersedes=match))
            continue

        merged = _merge_evidence(c, match)
        results.append(DedupResult(candidate=merged, action="merged", merged_into=match))
    return results


# ── result type ───────────────────────────────────────────────────────────


class DedupResult:
    """Outcome of deduplication for one candidate."""

    __slots__ = ("candidate", "action", "merged_into", "supersedes")

    def __init__(
        self,
        candidate: MemoryCandidate | None,
        action: str,
        merged_into: dict | None = None,
        supersedes: dict | None = None,
    ) -> None:
        self.candidate = candidate
        self.action = action  # "new" | "merged" | "supersedes"
        self.merged_into = merged_into
        self.supersedes = supersedes


# ── internal helpers ──────────────────────────────────────────────────────


def _fingerprint(candidate: MemoryCandidate) -> str:
    """A content-based fingerprint for similarity matching."""
    normalised = candidate.title.lower().strip()[:100] + "|" + _normalise(candidate.content)[:300]
    return sha256(normalised.encode()).hexdigest()


def _normalise(text: str) -> str:
    """Strip whitespace, lower-case, collapse runs of spaces."""
    import re
    return re.sub(r"\s+", " ", text.lower().strip())


# Same duplicate threshold as memory-intelligence's deterministic merge
# (findMergeCandidate): the shorter statement's content words are almost all
# in the other. The comparison is polarity-aware (statements.py): a
# statement and its negation are never duplicates.
DUPLICATE_OVERLAP = 0.85


def _overlap(a: str, b: str) -> float:
    return duplicate_score(a, b)


def _find_match(fp: str, existing: Sequence[dict], content: str = "") -> dict | None:
    """The existing record this candidate duplicates, if any.

    An identical title and content always matches; otherwise the closest
    record whose proposition overlap reaches DUPLICATE_OVERLAP, which does
    not contradict the candidate, and of which one says nothing the other
    does not (memory-intelligence's findMergeCandidate applies the same
    rule). A restatement with a changed value is a new statement, not a
    duplicate: merging it would keep the old value and drop the new one.
    """
    best: tuple[float, dict] | None = None
    for row in existing:
        existing_content = _normalise(row.get("content", ""))
        existing_title = row.get("title", "").lower().strip()[:100]
        combined = sha256((existing_title + "|" + existing_content[:300]).encode()).hexdigest()
        if combined == fp:
            return row
        if content:
            score = _overlap(content, row.get("content", ""))
            if (score >= DUPLICATE_OVERLAP and restates(content, row.get("content", ""))
                    and (best is None or score > best[0])):
                best = (score, row)
    return best[1] if best else None


def _outranks(candidate: MemoryCandidate, existing: dict) -> bool:
    """A candidate supersedes an existing record it significantly outranks."""
    existing_authority = existing.get("authority_level", 0)
    candidate_authority = candidate.authority_level
    return candidate_authority >= existing_authority + 20  # significant gap


def _merge_evidence(
    candidate: MemoryCandidate,
    existing: dict,
) -> MemoryCandidate:
    """Merge evidence from the candidate into the existing record.

    Duplicate evidence references (same kind + reference_id) are skipped.
    """
    existing_evidence = existing.get("evidence", [])
    existing_refs: set[tuple[str, str]] = set()
    for ev in existing_evidence:
        if isinstance(ev, dict):
            existing_refs.add((ev.get("kind", ""), ev.get("reference_id", "")))

    merged_evidence: list[EvidenceReference] = list(existing_evidence)
    for ev in candidate.evidence:
        key = (ev.kind, ev.reference_id)
        if key not in existing_refs:
            merged_evidence.append(ev)
            existing_refs.add(key)

    candidate.evidence = merged_evidence
    return candidate
