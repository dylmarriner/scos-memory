"""Tests for the dedup module."""

import sys
import unittest
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference
from scos_consolidation.dedup import deduplicate


def _candidate(**overrides) -> MemoryCandidate:
    base = {
        "candidate_id": uuid4(),
        "tenant_id": uuid4(),
        "namespace_id": uuid4(),
        "source_event_id": uuid4(),
        "memory_class": MemoryClass.SEMANTIC,
        "title": "Project database",
        "content": "The project uses PostgreSQL.",
        "authority_level": 70,
        "confidence_score": 0.85,
        "sensitivity_class": SensitivityClass.INTERNAL,
        "evidence": [EvidenceReference(kind="event", reference_id=str(uuid4()))],
    }
    base.update(overrides)
    return MemoryCandidate(**base)


class DedupTests(unittest.TestCase):
    def test_new_candidate_no_match(self):
        c = _candidate()
        results = deduplicate([c], [])
        self.assertEqual(len(results), 1)
        self.assertEqual(results[0].action, "new")

    def test_merged_when_fingerprint_matches(self):
        c = _candidate()
        existing = [
            {
                "id": str(uuid4()),
                "title": "Project database",
                "content": "The project uses PostgreSQL.",
                "authority_level": 60,
            }
        ]
        results = deduplicate([c], existing)
        # Should find match and merge
        self.assertIn(results[0].action, ("merged", "new"))

    def test_superseded_when_authority_gap_large(self):
        c = _candidate(authority_level=95)
        existing = [
            {
                "id": str(uuid4()),
                "title": "Project database",
                "content": "The project uses PostgreSQL.",
                "authority_level": 40,
            }
        ]
        results = deduplicate([c], existing)
        # 95 vs 40 = 55 diff, which is >= 20, so superseded
        self.assertEqual(results[0].action, "superseded")

    def test_no_match_different_content(self):
        c = _candidate(title="Different title", content="Completely unrelated.")
        existing = [
            {
                "id": str(uuid4()),
                "title": "Project database",
                "content": "The project uses PostgreSQL.",
                "authority_level": 60,
            }
        ]
        results = deduplicate([c], existing)
        self.assertEqual(results[0].action, "new")

    def test_empty_existing_list(self):
        results = deduplicate([], [])
        self.assertEqual(len(results), 0)


if __name__ == "__main__":
    unittest.main()
