"""Tests for the conflict detection module."""

import sys
import unittest
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference
from scos_consolidation.conflicts import detect_conflicts, extract_claims


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

    # Turn-derived memories have per-turn titles, so these deliberately use
    # unrelated titles: subjects must be matched from the statements.
    def _row(self, content, authority=70):
        return {"id": str(uuid4()), "title": "turn/" + str(uuid4()), "content": content, "authority_level": authority}

    def test_direct_negation_across_turns(self):
        c = _candidate(title="Knowledge: a", content="[user]\nThe billing service does not use Redis.")
        conflicts = detect_conflicts([c], [self._row("[user]\nThe billing service uses Redis.")])
        self.assertEqual(len(conflicts), 1)
        self.assertTrue(any("Contradictory statement" in s for s in conflicts[0].statements))
        self.assertEqual(conflicts[0].subjects[0], str(c.source_event_id))

    def test_single_valued_attribute_conflict(self):
        c = _candidate(title="Knowledge: a", content="The billing service runs on db02.")
        conflicts = detect_conflicts([c], [self._row("The billing service uses PostgreSQL 17 and runs on db01.")])
        self.assertEqual(len(conflicts), 1)
        self.assertTrue(any("Mutually exclusive" in s for s in conflicts[0].statements))

    def test_multi_valued_predicate_is_not_a_conflict(self):
        c = _candidate(title="Knowledge: a", content="The billing service uses Redis.")
        self.assertEqual(detect_conflicts([c], [self._row("The billing service uses PostgreSQL.")]), [])

    def test_unrelated_subjects_do_not_conflict(self):
        c = _candidate(title="Knowledge: a", content="The ledger service runs on db02.")
        self.assertEqual(detect_conflicts([c], [self._row("The billing service runs on db01.")]), [])

    def test_pronoun_subjects_are_ignored(self):
        c = _candidate(title="Knowledge: a", content="It runs on db02.")
        self.assertEqual(detect_conflicts([c], [self._row("It runs on db01.")]), [])

    def test_no_longer_is_a_negation(self):
        claims = extract_claims("The billing service no longer runs on db01 but runs on db02.")
        self.assertEqual([tuple(c) for c in claims], [
            ("billing service", "runs on", "db01", True),
            ("billing service", "runs on", "db02", False),
        ])

    def test_hostnames_and_ports_survive_parsing(self):
        claims = extract_claims("The cache isn't located at 10.0.0.1:6379. db01.example.com runs on Ubuntu 24.04")
        self.assertEqual([tuple(c) for c in claims], [
            ("cache", "located at", "10.0.0.1:6379", True),
            ("db01.example.com", "runs on", "ubuntu 24.04", False),
        ])


if __name__ == "__main__":
    unittest.main()
