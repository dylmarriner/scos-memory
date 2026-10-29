"""Python benchmark metrics package (§33)."""

from __future__ import annotations

import math


def recall_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    """Recall of unique relevant evidence; repeated labels are not extra truth."""
    relevant = set(expected)
    if not relevant or k <= 0:
        return 0.0
    return len(relevant & set(retrieved[:k])) / len(relevant)


def precision_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    """Distinct relevant hits per returned slot, including duplicate slots."""
    if k <= 0:
        return 0.0
    slots = retrieved[:k]
    if not slots:
        return 0.0
    return len(set(expected) & set(slots)) / len(slots)


def mean_reciprocal_rank(expected: list[str], retrieved: list[str]) -> float:
    exp_set = set(expected)
    for i, r in enumerate(retrieved):
        if r in exp_set:
            return 1.0 / (i + 1)
    return 0.0


def ndcg_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    """Binary nDCG; each unique relevant document earns gain at most once."""
    relevant = set(expected)
    if not relevant or k <= 0:
        return 0.0
    dcg = 0.0
    seen: set[str] = set()
    for i, r in enumerate(retrieved[:k]):
        if r in relevant and r not in seen:
            dcg += 1.0 / math.log2(i + 2)
        seen.add(r)
    idcg = sum(1.0 / math.log2(i + 2) for i in range(min(k, len(relevant))))
    return dcg / idcg if idcg else 0.0
