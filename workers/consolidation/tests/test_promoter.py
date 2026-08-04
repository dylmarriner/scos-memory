"""Tests for the promoter module."""

import sys
import unittest
from hashlib import sha256
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference
from scos_consolidation.promoter import (
    evaluate_promotion,
    build_memory_item,
    build_memory_version,
    PromotionDecision,
)


def _candidate(**overrides) -> MemoryCandidate:
    base = {
        "candidate_id": uuid4(),
        "tenant_id": uuid4(),
        "namespace_id": uuid4(),
        "source_event_id": uuid4(),
        "memory_class": MemoryClass.SEMANTIC,
        "title": "Test knowledge",
        "content": "This is test content.",
        "authority_level": 70,
        "confidence_score": 0.85,
        "sensitivity_class": SensitivityClass.INTERNAL,
        "evidence": [EvidenceReference(kind="event", reference_id=str(uuid4()))],
        "metadata": {"actor_id": str(uuid4())},
    }
    base.update(overrides)
    return MemoryCandidate(**base)


class PromoterTests(unittest.TestCase):
    def test_promote_standard_candidate(self):
        c = _candidate()
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "promote")
        self.assertEqual(decision.target_status, "active")

    def test_reject_low_confidence(self):
        c = _candidate(confidence_score=0.30)
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "reject")

    def test_reject_low_authority(self):
        c = _candidate(authority_level=10)
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "reject")

    def test_review_on_conflict(self):
        c = _candidate()
        conflicts = [
            type("FakeConflict", (), {"conflict_id": uuid4()})()
        ]
        decision = evaluate_promotion(c, conflicts)
        self.assertEqual(decision.decision, "review")
        self.assertTrue(decision.requires_human)

    def test_review_governing_class(self):
        c = _candidate(memory_class=MemoryClass.GOVERNING)
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "review")
        self.assertTrue(decision.requires_human)

    def test_review_revocation_class(self):
        c = _candidate(memory_class=MemoryClass.REVOCATION)
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "review")
        self.assertTrue(decision.requires_human)

    def test_review_sensitive_semantic(self):
        c = _candidate(
            memory_class=MemoryClass.SEMANTIC,
            sensitivity_class=SensitivityClass.CONFIDENTIAL,
        )
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "review")

    def test_review_high_impact_content(self):
        c = _candidate(content="This involves financial transactions.")
        decision = evaluate_promotion(c, [])
        self.assertEqual(decision.decision, "review")

    def test_build_memory_item_contains_expected_keys(self):
        c = _candidate()
        decision = PromotionDecision(
            candidate_id=c.candidate_id,
            decision="promote",
            target_status="active",
        )
        item = build_memory_item(c, decision)
        self.assertEqual(item["id"], c.candidate_id)
        self.assertEqual(item["status"], "active")
        self.assertEqual(item["type"], "semantic")

    def test_build_memory_version_contains_version_1(self):
        c = _candidate()
        decision = PromotionDecision(
            candidate_id=c.candidate_id,
            decision="promote",
            target_status="active",
        )
        item = build_memory_item(c, decision)
        version = build_memory_version(c, item["id"])
        self.assertEqual(version["version_number"], 1)
        self.assertEqual(version["memory_id"], item["id"])

    def test_build_memory_version_hashes_content(self):
        c = _candidate(content="Hash this content")

        version = build_memory_version(c, c.candidate_id)

        self.assertEqual(
            version["content_hash"],
            sha256(c.content.encode("utf-8")).hexdigest(),
        )
        self.assertNotEqual(version["content_hash"], c.content)


if __name__ == "__main__":
    unittest.main()
