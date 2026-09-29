"""Python benchmark metrics package (§33)."""

from __future__ import annotations

import math


def _hits(expected: list[str], retrieved: list[str], k: int) -> tuple[list[bool], int]:
    """Hit flags for the top k results, crediting each expected ID only at
    its first occurrence, plus the number of distinct expected IDs."""
    want = set(expected)
    relevant = len(want)
    hits = []
    for r in retrieved[: max(k, 0)]:
        hits.append(r in want)
        want.discard(r)
    return hits, relevant


def recall_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    hits, relevant = _hits(expected, retrieved, k)
    return sum(hits) / relevant if relevant else 0.0


def precision_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    hits, _ = _hits(expected, retrieved, k)
    return sum(hits) / len(hits) if hits else 0.0


def mean_reciprocal_rank(expected: list[str], retrieved: list[str]) -> float:
    exp_set = set(expected)
    for i, r in enumerate(retrieved):
        if r in exp_set:
            return 1.0 / (i + 1)
    return 0.0


def ndcg_at_k(expected: list[str], retrieved: list[str], k: int) -> float:
    """Binary-relevance nDCG@k; the ideal ranking uses k, not the number of
    results returned."""
    hits, relevant = _hits(expected, retrieved, k)
    dcg = sum(1.0 / math.log2(i + 2) for i, h in enumerate(hits) if h)
    idcg = sum(1.0 / math.log2(i + 2) for i in range(min(max(k, 0), relevant)))
    return dcg / idcg if idcg > 0 else 0.0
