"""Tests for the conflict detection module."""

import sys
import unittest
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference
from scos_consolidation.conflicts import detect_conflicts


def _candidate(**overrides) -> MemoryCandidate:
    base = {
        "candidate_id": uuid4(),
        "tenant_id": uuid4(),
        "namespace_id": uuid4(),
        "source_event_id": uuid4(),
        "memory_class": MemoryClass.SEMANTIC,
        "title": "Knowledge: Database engine",
        "content": "The project uses PostgreSQL.",
        "authority_level": 70,
        "confidence_score": 0.85,
        "sensitivity_class": SensitivityClass.INTERNAL,
        "evidence": [EvidenceReference(kind="event", reference_id=str(uuid4()))],
    }
    base.update(overrides)
    return MemoryCandidate(**base)


class ConflictTests(unittest.TestCase):
    def test_no_conflict_on_same_subject_same_claim(self):
        c = _candidate(title="Database engine", content="The project uses PostgreSQL.")
        existing = [
            {
                "id": str(uuid4()),
                "title": "Database engine",
                "content": "The project uses PostgreSQL.",
                "authority_level": 70,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 0)

    def test_conflict_on_authority_mismatch(self):
        c = _candidate(title="Database engine", content="The project uses PostgreSQL.", authority_level=30)
        existing = [
            {
                "id": str(uuid4()),
                "title": "Database engine",
                "content": "The project uses PostgreSQL with MySQL.",
                "authority_level": 90,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        # 90 - 30 = 60 >= 40 → authority mismatch conflict
        self.assertGreaterEqual(len(conflicts), 1)

    def test_no_conflict_different_subjects(self):
        c = _candidate(title="Database engine", content="PostgreSQL")
        existing = [
            {
                "id": str(uuid4()),
                "title": "Deployment target",
                "content": "The project deploys to Kubernetes.",
                "authority_level": 80,
            }
        ]
        conflicts = detect_conflicts([c], existing)
        self.assertEqual(len(conflicts), 0)

    def test_no_conflict_empty_existing(self):
        c = _candidate(title="Database engine", content="PostgreSQL")
        conflicts = detect_conflicts([c], [])
        self.assertEqual(len(conflicts), 0)

    def test_empty_candidates_no_conflicts(self):
        conflicts = detect_conflicts([], [{"id": str(uuid4()), "title": "x", "content": "y"}])
        self.assertEqual(len(conflicts), 0)


if __name__ == "__main__":
    unittest.main()
