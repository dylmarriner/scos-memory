"""Tests for the review module."""

import sys
import unittest
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.review import (
    requires_human_review,
    apply_review,
    ReviewDecision,
)


class ReviewTests(unittest.TestCase):
    def test_governing_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "governing",
            "sensitivity": "internal",
            "content": "A policy change.",
            "title": "Access policy update",
        })
        self.assertTrue(needs)
        self.assertIn("governing", reason)

    def test_revocation_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "revocation",
            "sensitivity": "internal",
            "content": "Revoke access.",
            "title": "Revocation notice",
        })
        self.assertTrue(needs)

    def test_security_content_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "semantic",
            "sensitivity": "internal",
            "content": "Normal content.",
            "title": "Permission update requested",
        })
        self.assertTrue(needs)

    def test_destructive_content_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "semantic",
            "sensitivity": "internal",
            "content": "We need to delete the old configuration.",
            "title": "Cleanup task",
        })
        self.assertTrue(needs)

    def test_high_impact_content_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "semantic",
            "sensitivity": "internal",
            "content": "The financial report is ready.",
            "title": "Report",
        })
        self.assertTrue(needs)

    def test_high_sensitivity_requires_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "semantic",
            "sensitivity": "restricted",
            "content": "Normal content.",
            "title": "Normal title",
        })
        self.assertTrue(needs)

    def test_normal_candidate_no_review(self):
        needs, reason = requires_human_review({
            "proposed_type": "semantic",
            "sensitivity": "internal",
            "content": "The project uses PostgreSQL.",
            "title": "Database info",
        })
        self.assertFalse(needs)

    def test_apply_review_approved(self):
        decision = apply_review(
            candidate_id=uuid4(),
            decision="approved",
            rationale="Looks correct.",
        )
        self.assertEqual(decision.decision, "approved")
        self.assertIsNotNone(decision.review_id)

    def test_apply_review_rejected(self):
        decision = apply_review(
            candidate_id=uuid4(),
            decision="rejected",
            rationale="Inaccurate.",
        )
        self.assertEqual(decision.decision, "rejected")

    def test_invalid_decision_raises(self):
        with self.assertRaises(ValueError):
            apply_review(
                candidate_id=uuid4(),
                decision="invalid",
                rationale="nope",
            )


if __name__ == "__main__":
    unittest.main()
