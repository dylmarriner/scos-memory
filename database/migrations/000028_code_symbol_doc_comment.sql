BEGIN;

-- Real Go declaration extraction (goextract.go) captures each
-- function/type/const/var's doc comment, which the schema had nowhere to
-- put -- code_symbol_versions only ever described a whole file (the
-- heuristic Tier 0 extractor's only unit) until now. Left out of the
-- generated search_vector column deliberately, as a separate scoped
-- follow-up rather than folding it in here.
ALTER TABLE code_symbol_versions ADD COLUMN IF NOT EXISTS doc_comment text;

COMMIT;
