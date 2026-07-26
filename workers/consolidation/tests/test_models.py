import sys
import unittest
from pathlib import Path
from uuid import uuid4

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from scos_consolidation.models import (  # noqa: E402
    EvidenceReference,
    MemoryCandidate,
    MemoryClass,
    SensitivityClass,
)


class MemoryCandidateTests(unittest.TestCase):
    def base(self) -> dict:
        return {
            "candidate_id": uuid4(),
            "tenant_id": uuid4(),
            "namespace_id": uuid4(),
            "source_event_id": uuid4(),
            "memory_class": MemoryClass.SEMANTIC,
            "title": "Project database",
            "content": "The project uses PostgreSQL.",
            "authority_level": 70,
            "confidence_score": 0.95,
            "sensitivity_class": SensitivityClass.INTERNAL,
            "evidence": [EvidenceReference(kind="event", reference_id=str(uuid4()))],
        }

    def test_valid_candidate(self) -> None:
        candidate = MemoryCandidate(**self.base())
        self.assertEqual(candidate.memory_class, MemoryClass.SEMANTIC)

    def test_durable_candidate_requires_evidence(self) -> None:
        values = self.base()
        values["evidence"] = []
        with self.assertRaises(ValueError):
            MemoryCandidate(**values)

    def test_secret_candidate_is_rejected(self) -> None:
        values = self.base()
        values["sensitivity_class"] = SensitivityClass.SECRET
        with self.assertRaises(ValueError):
            MemoryCandidate(**values)


if __name__ == "__main__":
    unittest.main()
