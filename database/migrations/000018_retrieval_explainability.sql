BEGIN;

ALTER TABLE retrieval_runs
    ADD COLUMN IF NOT EXISTS candidate_counts jsonb NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS token_estimate integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS model_versions jsonb NOT NULL DEFAULT '{}'::jsonb;

COMMIT;
