"""Promotion gate: candidate → active memory.

Per §16: "Promotion requires namespace, source, type, authority,
confidence, sensitivity, retention, content hash and conflict assessment."

Human approval is required for:
- governing policy
- security changes
- permission expansion
- destructive procedures
- sensitive inferred traits
- organisation-wide promotion
- high-impact financial / legal / messaging / production
"""

from __future__ import annotations

import json
import logging
from datetime import datetime, timezone
from typing import Any
from uuid import UUID

from .models import MemoryCandidate, MemoryClass, SensitivityClass

logger = logging.getLogger(__name__)


# ── constants ─────────────────────────────────────────────────────────────


MIN_CONFIDENCE_FOR_PROMOTION = 0.60
MIN_AUTHORITY_FOR_PROMOTION = 30

HIGH_IMPACT_KEYWORDS = frozenset({
    "financial", "legal", "messaging", "production", "deploy",
    "payment", "contract", "compliance", "regulatory",
})

HUMAN_REVIEW_CLASSES = frozenset({
    MemoryClass.GOVERNING,
    MemoryClass.REVOCATION,
})


class PromotionDecision:
    """Result of evaluating a candidate for promotion.

    Attributes
    ----------
    candidate_id:
        The candidate being evaluated.
    decision:
        ``promote``, ``review``, or ``reject``.
    reason:
        Human-readable explanation.
    target_status:
        The status to set in memory_items if promoted.
    requires_human:
        Whether human approval is needed before final promotion.
    """

    __slots__ = ("candidate_id", "decision", "reason", "target_status", "requires_human")

    def __init__(
        self,
        candidate_id: UUID,
        decision: str,
        reason: str = "",
        target_status: str = "candidate",
        requires_human: bool = False,
    ) -> None:
        self.candidate_id = candidate_id
        self.decision = decision
        self.reason = reason
        self.target_status = target_status
        self.requires_human = requires_human

    def to_dict(self) -> dict[str, Any]:
        return {
            "candidate_id": str(self.candidate_id),
            "decision": self.decision,
            "reason": self.reason,
            "target_status": self.target_status,
            "requires_human": self.requires_human,
        }


# ── public API ────────────────────────────────────────────────────────────


def evaluate_promotion(
    candidate: MemoryCandidate,
    conflicts: list[Any],  # ConflictSet objects
) -> PromotionDecision:
    """Evaluate whether a candidate should be promoted to active memory.

    Returns one of:
    - ``promote`` — ready for immediate insertion as ``active``.
    - ``review`` — requires human review before promotion.
    - ``reject`` — does not meet minimum thresholds.
    """
    candidate_id = candidate.candidate_id

    # ── threshold checks → reject ────────────────────────────────────
    if candidate.confidence_score < MIN_CONFIDENCE_FOR_PROMOTION:
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="reject",
            reason=f"Confidence {candidate.confidence_score:.2f} < minimum {MIN_CONFIDENCE_FOR_PROMOTION}",
            target_status="candidate",
        )

    if candidate.authority_level < MIN_AUTHORITY_FOR_PROMOTION:
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="reject",
            reason=f"Authority {candidate.authority_level} < minimum {MIN_AUTHORITY_FOR_PROMOTION}",
            target_status="candidate",
        )

    # ── conflict check → review ──────────────────────────────────────
    if conflicts:
        conflict_ids = [str(c.conflict_id) for c in conflicts]
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="review",
            reason=f"Conflicts detected: {', '.join(conflict_ids)}",
            target_status="candidate",
            requires_human=True,
        )

    # ── class-based review requirements ──────────────────────────────
    if candidate.memory_class in HUMAN_REVIEW_CLASSES:
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="review",
            reason=f"Memory class {candidate.memory_class.value} requires human approval",
            target_status="candidate",
            requires_human=True,
        )

    if candidate.memory_class == MemoryClass.SEMANTIC and candidate.sensitivity_class in (
        SensitivityClass.CONFIDENTIAL,
        SensitivityClass.RESTRICTED,
    ):
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="review",
            reason=f"Sensitive semantic knowledge ({candidate.sensitivity_class.value}) requires human approval",
            target_status="candidate",
            requires_human=True,
        )

    # ── high-impact content check → review ───────────────────────────
    if any(kw in candidate.content.lower() for kw in HIGH_IMPACT_KEYWORDS):
        return PromotionDecision(
            candidate_id=candidate_id,
            decision="review",
            reason="High-impact content detected — human approval required",
            target_status="candidate",
            requires_human=True,
        )

    # ── promote ──────────────────────────────────────────────────────
    return PromotionDecision(
        candidate_id=candidate_id,
        decision="promote",
        reason="All thresholds met, no conflicts, no review required",
        target_status="active",
        requires_human=False,
    )


def build_memory_item(
    candidate: MemoryCandidate,
    decision: PromotionDecision,
) -> dict[str, Any]:
    """Build the row dict for inserting into memory_items."""
    now = datetime.now(timezone.utc)
    return {
        "id": candidate.candidate_id,
        "tenant_id": candidate.tenant_id,
        "namespace_id": candidate.namespace_id,
        "owner_principal_id": candidate.metadata.get("actor_id"),
        "type": candidate.memory_class.value,
        "status": decision.target_status,
        "title": candidate.title,
        "canonical_summary": candidate.content[:512],
        "authority_level": candidate.authority_level,
        "confidence_score": candidate.confidence_score,
        "sensitivity": candidate.sensitivity_class.value,
        "valid_from": candidate.valid_from,
        "valid_until": candidate.valid_until,
        "expires_at": candidate.expires_at,
        "metadata": json.dumps(candidate.metadata),
    }


def build_memory_version(
    candidate: MemoryCandidate,
    memory_item_id: UUID,
) -> dict[str, Any]:
    """Build the row dict for inserting the first memory_versions row."""
    from uuid import uuid4
    now = datetime.now(timezone.utc)
    return {
        "id": uuid4(),
        "memory_id": memory_item_id,
        "version_number": 1,
        "content": candidate.content,
        "structured_content": json.dumps({
            "title": candidate.title,
            "evidence": [ev.model_dump() for ev in candidate.evidence],
        }),
        "content_hash": candidate.content,
        "change_reason": "initial consolidation",
        "created_by": candidate.metadata.get("actor_id", UUID("00000000-0000-0000-0000-000000000000")),
        "created_at": now,
    }
