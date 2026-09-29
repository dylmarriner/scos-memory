BEGIN;

-- §26: PostgreSQL SHALL store the "commit hash, path, metadata, content hash
-- and import status" of indexed OKF content. okf_concepts (000011) had no
-- metadata column, so a document's front-matter metadata was never stored.
ALTER TABLE okf_concepts ADD COLUMN IF NOT EXISTS metadata jsonb NOT NULL DEFAULT '{}'::jsonb;

COMMIT;
