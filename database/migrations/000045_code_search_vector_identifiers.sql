BEGIN;

-- Code search could only match a symbol's qualified name and path, token
-- for token: "authenticate" never found resolveIdentity's doc comment,
-- "request handler" never found handleRequest, and any natural-language
-- question returned nothing. The search vector now also carries the
-- symbol's signature and doc comment, with camelCase/PascalCase
-- identifiers split into words and English stemming applied. Name and path
-- tokens keep weight A, so a direct name match still outranks a mention in
-- prose (weight B).
DROP INDEX IF EXISTS code_symbol_versions_search_idx;
ALTER TABLE code_symbol_versions DROP COLUMN IF EXISTS search_vector;
ALTER TABLE code_symbol_versions
    ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('simple', regexp_replace(
            coalesce(fully_qualified_name, '') || ' ' || coalesce(path, ''),
            '[/._-]', ' ', 'g'
        )), 'A')
        ||
        setweight(to_tsvector('english', regexp_replace(regexp_replace(
            coalesce(fully_qualified_name, '') || ' ' || coalesce(signature, '') || ' ' || coalesce(doc_comment, ''),
            '([a-z0-9])([A-Z])', '\1 \2', 'g'),
            '[^A-Za-z0-9]+', ' ', 'g'
        )), 'B')
    ) STORED;
CREATE INDEX code_symbol_versions_search_idx ON code_symbol_versions USING GIN (search_vector);

COMMIT;
