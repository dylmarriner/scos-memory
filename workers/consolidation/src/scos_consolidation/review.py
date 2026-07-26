"""Human-approval review workflow for memory candidates.

Per §16: "Human approval SHALL be required for governing policy, security
changes, permission expansion, destructive procedures, sensitive inferred
traits, organisation-wide promotion and high-impact financial, legal,
messaging or production procedures."
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone
from typing import Any
from uuid import UUID, uuid4

logger = logging.getLogger(__name__)


# ── decision types ────────────────────────────────────────────────────────


class ReviewDecision:
    """Immutable record of a review decision on one candidate."""

    __slots__ = (
        "review_id", "candidate_id", "reviewer_id",
        "decision", "rationale", "created_at",
    )

    def __init__(
        self,
        candidate_id: UUID,
        reviewer_id: UUID | None,
        decision: str,
        rationale: str,
    ) -> None:
        if decision not in ("approved", "rejected", "quarantined", "needs_changes"):
            raise ValueError(f"Invalid review decision: {decision}")
        self.review_id = uuid4()
        self.candidate_id = candidate_id
        self.reviewer_id = reviewer_id
        self.decision = decision
        self.rationale = rationale
        self.created_at = datetime.now(timezone.utc)

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": str(self.review_id),
            "candidate_id": str(self.candidate_id),
            "reviewer_id": str(self.reviewer_id) if self.reviewer_id else None,
            "decision": self.decision,
            "rationale": self.rationale,
            "created_at": self.created_at.isoformat(),
        }


# ── review evaluators ─────────────────────────────────────────────────────


def requires_human_review(
    candidate_dict: dict[str, Any],
) -> tuple[bool, str]:
    """Determine if a candidate requires human review before promotion.

    Parameters
    ----------
    candidate_dict:
        The memory_candidates row as a dict.

    Returns
    -------
    (requires_review, reason)
    """
    candidate_type = candidate_dict.get("proposed_type", "")
    sensitivity = candidate_dict.get("sensitivity", "internal")
    content = candidate_dict.get("content", "").lower()
    title = candidate_dict.get("title", "").lower()

    # Governing / policy
    if candidate_type in ("governing", "revocation"):
        return True, f"Memory class '{candidate_type}' requires human approval"

    # Security / permission changes
    if any(kw in title for kw in ("permission", "security", "access", "role")):
        return True, "Security or permission content requires human approval"

    # Destructive procedures
    if any(kw in content for kw in ("delete", "remove", "destroy", "terminate")):
        return True, "Destructive procedure content requires human approval"

    # Sensitive inferred traits
    if any(kw in content for kw in ("personality", "behaviour", "tendency", "preference")):
        return True, "Inferred behavioural traits require human approval"

    # High-impact categories
    high_impact = ("financial", "legal", "messaging", "production", "payment",
                   "contract", "compliance", "regulatory", "organisation-wide",
                   "org-wide")
    if any(kw in content for kw in high_impact):
        return True, "High-impact content requires human approval"

    # High sensitivity
    if sensitivity in ("restricted", "secret", "confidential"):
        return True, f"Sensitivity class '{sensitivity}' requires human approval"

    return False, ""


def apply_review(
    candidate_id: UUID,
    decision: str,
    rationale: str,
    reviewer_id: UUID | None = None,
) -> ReviewDecision:
    """Create a review decision record.

    The caller is responsible for persisting this to the ``memory_reviews``
    table and updating the candidate's status.
    """
    return ReviewDecision(
        candidate_id=candidate_id,
        reviewer_id=reviewer_id,
        decision=decision,
        rationale=rationale,
    )


REVIEW_DECISION_TRANSITIONS: dict[str, str] = {
    "approved": "approved",
    "rejected": "rejected",
    "quarantined": "quarantined",
    "needs_changes": "candidate",  # stays in candidate state pending revision
}
