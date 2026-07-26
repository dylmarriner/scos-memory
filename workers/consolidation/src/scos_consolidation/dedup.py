"""Duplicate detection and evidence merging for memory candidates.

Per §16: "Duplicates SHALL merge evidence rather than produce endless
paraphrases."
"""

from __future__ import annotations

import logging
from hashlib import sha256
from typing import Sequence

from .models import EvidenceReference, MemoryCandidate

logger = logging.getLogger(__name__)


# ── public API ────────────────────────────────────────────────────────────


def deduplicate(
    candidates: list[MemoryCandidate],
    existing: Sequence[dict],
) -> list[DedupResult]:
    """Classify each candidate as *new*, *merged*, or *superseded*.

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
        match = _find_match(fp, existing)
        if match is None:
            results.append(DedupResult(candidate=c, action="new"))
            continue

        if _is_superseded(c, match):
            results.append(DedupResult(candidate=c, action="superseded", superseded_by=match))
            continue

        merged = _merge_evidence(c, match)
        results.append(DedupResult(candidate=merged, action="merged", merged_into=match))
    return results


# ── result type ───────────────────────────────────────────────────────────


class DedupResult:
    """Outcome of deduplication for one candidate."""

    __slots__ = ("candidate", "action", "merged_into", "superseded_by")

    def __init__(
        self,
        candidate: MemoryCandidate | None,
        action: str,
        merged_into: dict | None = None,
        superseded_by: dict | None = None,
    ) -> None:
        self.candidate = candidate
        self.action = action  # "new" | "merged" | "superseded"
        self.merged_into = merged_into
        self.superseded_by = superseded_by


# ── internal helpers ──────────────────────────────────────────────────────


def _fingerprint(candidate: MemoryCandidate) -> str:
    """A content-based fingerprint for similarity matching."""
    normalised = candidate.title.lower().strip()[:100] + "|" + _normalise(candidate.content)[:300]
    return sha256(normalised.encode()).hexdigest()


def _normalise(text: str) -> str:
    """Strip whitespace, lower-case, collapse runs of spaces."""
    import re
    return re.sub(r"\s+", " ", text.lower().strip())


def _find_match(fp: str, existing: Sequence[dict]) -> dict | None:
    """Find an existing record whose fingerprint overlaps."""
    for row in existing:
        existing_content = _normalise(row.get("content", ""))
        existing_title = row.get("title", "").lower().strip()[:100]
        combined = sha256((existing_title + "|" + existing_content[:300]).encode()).hexdigest()
        if combined == fp:
            return row
    return None


def _is_superseded(candidate: MemoryCandidate, existing: dict) -> bool:
    """A candidate supersedes existing if it has higher authority."""
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
