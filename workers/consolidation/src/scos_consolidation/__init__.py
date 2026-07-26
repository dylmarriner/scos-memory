"""SCOS governed memory consolidation package.

Sub-packages:
  extractor   — event-driven candidate extraction
  dedup       — evidence-merging duplicate detection
  conflicts   — contradiction detection and conflict sets
  promoter    — candidate-to-memory promotion gate
  review      — human-approval review workflow
  worker      — outbox-polling main loop
"""

from .models import MemoryCandidate, MemoryClass, SensitivityClass, EvidenceReference

__all__ = [
    "MemoryCandidate",
    "MemoryClass",
    "SensitivityClass",
    "EvidenceReference",
]
