BEGIN;

-- code_edges has carried confidence_class since migration 000025 (blueprint
-- §2.4: "every edge or claim must include an epistemic class"), but
-- code_symbol_versions never did -- there was nowhere to record that a
-- Tier 0 file-level symbol is a path heuristic, a goextract.go Go
-- declaration is static-analysis-derived, or a SCIP-ingested symbol is
-- deterministic (real type-checked semantic resolution from an external
-- indexer). All three now coexist in this table, so this stopped being
-- optional.
ALTER TABLE code_symbol_versions ADD COLUMN IF NOT EXISTS confidence_class text NOT NULL DEFAULT 'heuristic';
ALTER TABLE code_symbol_versions ADD COLUMN IF NOT EXISTS extractor text;

COMMIT;
