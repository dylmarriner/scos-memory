"""Polarity-aware comparison of short factual statements.

Deduplication and contradiction detection both ask whether two pieces of
text state the same proposition. They must agree on the answer, and both
must notice when one statement is the negation of the other: "the API is a
monolith" and "the API is not a monolith" share every content word, so a
plain word-overlap test calls them duplicates and merges a contradiction
into the record it contradicts.

A statement is reduced to its *proposition* -- the set of its content words,
lower-cased, with negation words, auxiliaries and articles removed and
simple inflections folded ("uses"/"use", "deploys"/"deploy") -- and its
*polarity*, negative when it carries an odd number of negations.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

# Negations, including contracted forms split off by the tokenizer
# ("doesn't" -> "doesn", "t" is dropped below as a one-letter token).
_NEGATIONS = frozenset({
    "not", "no", "never", "none", "nobody", "nothing", "neither", "nor", "without",
    "cannot", "can't", "dont", "don't", "doesnt", "doesn't", "didnt", "didn't",
    "isnt", "isn't", "arent", "aren't", "wasnt", "wasn't", "werent", "weren't",
    "wont", "won't", "shouldnt", "shouldn't", "mustnt", "mustn't", "hasnt", "hasn't",
    "havent", "haven't", "hadnt", "hadn't", "couldnt", "couldn't", "wouldnt", "wouldn't",
})

# Words that carry no proposition of their own.
_STOPWORDS = frozenset({
    "a", "an", "the", "is", "are", "was", "were", "be", "been", "being", "am",
    "do", "does", "did", "has", "have", "had", "will", "would", "shall", "should",
    "can", "could", "may", "might", "must", "to", "of", "and", "or", "that", "this",
    "these", "those", "it", "its", "we", "our", "us", "they", "their", "i", "my",
    "you", "your", "he", "she", "his", "her", "them", "on", "in", "at", "by", "for",
    "with", "as", "so", "any", "all", "also", "just", "still", "very", "really",
})

_TOKEN = re.compile(r"[a-z0-9][a-z0-9'._/:-]*[a-z0-9]|[a-z0-9]")
_SENTENCE = re.compile(r"(?<=[.!?;])\s+|\n+")


def _fold(word: str) -> str:
    """Fold simple English inflections so 'uses' and 'use' compare equal."""
    if len(word) > 4 and word.endswith("ies"):
        return word[:-3] + "y"
    if len(word) > 4 and word.endswith(("ses", "xes", "zes", "ches", "shes")):
        return word[:-2]
    if len(word) > 3 and word.endswith("s") and not word.endswith("ss"):
        return word[:-1]
    return word


@dataclass(frozen=True)
class Statement:
    proposition: frozenset[str]
    negative: bool
    text: str


def parse(text: str) -> Statement:
    """The proposition and polarity of one statement."""
    negations = 0
    words: set[str] = set()
    # "no longer" is a single negation.
    lowered = re.sub(r"\bno longer\b", "not", text.lower())
    for raw in _TOKEN.findall(lowered):
        token = raw.strip("'")
        if token in _NEGATIONS or token.endswith("n't"):
            negations += 1
            continue
        if token in _STOPWORDS or len(token) < 2:
            continue
        words.add(_fold(token))
    return Statement(proposition=frozenset(words), negative=negations % 2 == 1, text=text.strip())


def sentences(text: str) -> list[Statement]:
    """Each sentence of ``text`` that states something."""
    out = []
    for part in _SENTENCE.split(text):
        stmt = parse(part)
        if len(stmt.proposition) >= 2:
            out.append(stmt)
    return out


def similarity(a: Statement, b: Statement) -> float:
    """How much of the smaller proposition the larger one contains."""
    if not a.proposition or not b.proposition:
        return 0.0
    return len(a.proposition & b.proposition) / min(len(a.proposition), len(b.proposition))


def same_proposition(a: Statement, b: Statement, threshold: float) -> bool:
    return similarity(a, b) >= threshold


def contradictions(text_a: str, text_b: str, threshold: float = 0.85) -> list[tuple[str, str]]:
    """Sentence pairs that state the same proposition with opposite polarity."""
    found = []
    for sa in sentences(text_a):
        for sb in sentences(text_b):
            if sa.negative != sb.negative and same_proposition(sa, sb, threshold):
                found.append((sa.text, sb.text))
    return found


def duplicate_score(text_a: str, text_b: str) -> float:
    """How far ``text_a`` restates ``text_b``: the proposition overlap of the
    two texts, or 0 when any sentence of one contradicts a sentence of the
    other -- a negation is never a duplicate."""
    if contradictions(text_a, text_b):
        return 0.0
    return similarity(parse(text_a), parse(text_b))


def restates(text_a: str, text_b: str) -> bool:
    """Whether one text says nothing the other does not: a repeat, or one
    strictly richer than the other. Overlap alone would fold "the codename
    is tangerine-037ea5" into "the codename is tangerine-94c27f"; the one
    word that differs is the information."""
    a, b = parse(text_a).proposition, parse(text_b).proposition
    return a <= b or b <= a
