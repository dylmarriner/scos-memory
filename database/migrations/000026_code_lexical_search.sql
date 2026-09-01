BEGIN;

-- Lexical/BM25-class retrieval for code (blueprint §11's "Lexical
-- retrieval: BM25 / PostgreSQL FTS / trigram. Essential for identifiers
-- where embeddings can be embarrassingly imaginative."), and the
-- audit's "BM25 / lexical: Missing/Unknown" line item. PostgreSQL FTS
-- rather than a dedicated engine (Zoekt et al.) for the reason given
-- throughout the blueprint: one operational database covers this until
-- scale actually justifies a specialized adapter (§21).
--
-- 'simple' configuration deliberately skips English stemming/stopwords:
-- identifiers like "GetUserByID" or "http_client" are not English prose,
-- and stemming would either be a no-op on camelCase/snake_case tokens or
-- actively wrong (e.g. stripping a meaningful trailing "s").
--
-- The default word parser treats a whole path like "internal/auth/
-- middleware.go" as a single URL/file-shaped token rather than splitting
-- it on '/' and '.' (verified directly: `to_tsvector('simple',
-- 'internal/auth/middleware.go')` produces one token, not four) --
-- exactly backwards from what a code search needs, where a query for
-- "middleware" should find that path. Replacing path/identifier
-- separators with spaces before tokenizing forces each segment to become
-- its own token.
ALTER TABLE code_symbol_versions
    ADD COLUMN IF NOT EXISTS search_vector tsvector
    GENERATED ALWAYS AS (
        to_tsvector('simple', regexp_replace(
            coalesce(fully_qualified_name, '') || ' ' || coalesce(path, ''),
            '[/._-]', ' ', 'g'
        ))
    ) STORED;

CREATE INDEX IF NOT EXISTS code_symbol_versions_search_idx ON code_symbol_versions USING GIN (search_vector);

COMMIT;
