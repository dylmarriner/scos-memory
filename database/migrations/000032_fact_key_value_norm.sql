BEGIN;

-- P0-C (same-key-different-value contradiction check): memory_items had no
-- key/value decomposition of a fact's content, so isContradictory could
-- only catch negation-polarity conflicts ("X is up" vs "X is not up"), not
-- same-polarity value conflicts ("ships on Tuesdays" vs "ships on
-- Thursdays"). Nullable, additive: populated by a coarse heuristic in
-- insertFact (lowercase, strip stopwords, first N significant words as
-- key_norm, rest as value_norm), same spirit as the existing
-- isContradictory/findMergeCandidate heuristics.
ALTER TABLE memory_items ADD COLUMN IF NOT EXISTS key_norm text;
ALTER TABLE memory_items ADD COLUMN IF NOT EXISTS value_norm text;

COMMIT;
