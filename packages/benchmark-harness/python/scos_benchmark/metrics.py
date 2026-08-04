"""Python benchmark metrics package (§33)."""

from __future__ import annotations

import math


def recall_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    if not expected:
        return 0.0
    retrieved_k = set(retrieved[:k])
    hits = len(set(expected) & retrieved_k)
    return hits / len(expected)


def precision_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    if not retrieved[:k]:
        return 0.0
    retrieved_k = set(retrieved[:k])
    hits = len(set(expected) & retrieved_k)
    return hits / len(retrieved_k)


def mean_reciprocal_rank(expected: list[str], retrieved: list[str]) -> float:
    exp_set = set(expected)
    for i, r in enumerate(retrieved):
        if r in exp_set:
            return 1.0 / (i + 1)
    return 0.0


def ndcg_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    exp_set = set(expected)
    dcg = 0.0
    for i, r in enumerate(retrieved[:k]):
        rel = 1.0 if r in exp_set else 0.0
        dcg += rel / math.log2(i + 2) if i > 0 else rel

    idcg = 0.0
    for i in range(min(k, len(expected))):
        idcg += 1.0 / math.log2(i + 2) if i > 0 else 1.0

    return dcg / idcg if idcg > 0 else 0.0
