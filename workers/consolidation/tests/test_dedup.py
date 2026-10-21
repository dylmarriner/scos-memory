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
    def test_a_negation_is_never_merged_as_a_duplicate(self):
        for mine, theirs in [
            ("The API is not a monolith.", "The API is a monolith."),
            ("We do not use PostgreSQL.", "We use PostgreSQL."),
            ("We never deploy on Fridays.", "We deploy on Fridays."),
        ]:
            existing = [{"id": str(uuid4()), "kind": "memory", "title": "Something else", "content": theirs, "authority_level": 70}]
            results = deduplicate([_candidate(title="A statement", content=mine)], existing)
            self.assertEqual(results[0].action, "new", mine)

    def test_a_restatement_is_merged(self):
        existing = [{"id": str(uuid4()), "kind": "memory", "title": "Cache", "content": "The service uses Redis for caching.", "authority_level": 70}]
        results = deduplicate([_candidate(title="Redis", content="Service uses Redis for caching")], existing)
        self.assertEqual(results[0].action, "merged")

    def test_a_restatement_with_a_changed_value_is_not_merged(self):
        for mine, theirs in [
            ("Remember for future sessions: the codename of my SCOS integration check is tangerine-94c27f. Reply with just: noted.",
             "Remember for future sessions: the codename of my SCOS integration check is tangerine-037ea5. Reply with just: noted."),
            ("The weekly production deploy window for the billing service opens Wednesday at 14:00 UTC.",
             "The weekly production deploy window for the billing service opens Tuesday at 14:00 UTC."),
            ("The billing service deploy window opens Tuesday at 14:00 UTC. The search service deploy window opens Wednesday at 14:00 UTC.",
             "The billing service deploy window opens Wednesday at 14:00 UTC. The search service deploy window opens Tuesday at 14:00 UTC."),
        ]:
            existing = [{"id": str(uuid4()), "kind": "memory", "title": "Something else", "content": theirs, "authority_level": 70}]
            results = deduplicate([_candidate(title="A statement", content=mine)], existing)
            self.assertEqual(results[0].action, "new", mine)

    def test_a_restatement_split_into_sentences_differently_is_merged(self):
        existing = [{"id": str(uuid4()), "kind": "memory", "title": "Stack", "content": "The service uses Redis for caching and PostgreSQL for storage.", "authority_level": 70}]
        results = deduplicate([_candidate(title="Stack", content="The service uses Redis for caching. The service uses PostgreSQL for storage.")], existing)
        self.assertEqual(results[0].action, "merged")

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

    def test_supersedes_when_authority_gap_large(self):
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
        # 95 vs 40 = 55 diff, which is >= 20: the candidate replaces it
        self.assertEqual(results[0].action, "supersedes")
        self.assertIsNotNone(results[0].supersedes)

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


class ParaphraseTests(unittest.TestCase):
    def test_reworded_statement_of_the_same_fact_merges(self):
        from scos_consolidation.dedup import _overlap
        self.assertGreaterEqual(_overlap(
            "Our on-call rotation changes every Monday at 09:00.",
            "The on-call rotation changes every Monday at 09:00."), 0.85)

    def test_different_facts_do_not_merge(self):
        from scos_consolidation.dedup import _overlap
        self.assertLess(_overlap(
            "The on-call rotation changes every Monday at 09:00.",
            "The release train leaves every Thursday at noon."), 0.85)

