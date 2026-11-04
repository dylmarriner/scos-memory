BEGIN;

-- P6 evidence-link confidence is a probability, not an arbitrary float.
-- NOT VALID preserves existing rows for a separate read-only inventory and
-- approved remediation while enforcing the bound on all new/updated rows.
-- PostgreSQL's float ordering makes NaN fail the upper bound; infinities and
-- ordinary out-of-range values fail one of the two comparisons as well.
DO $guard$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'memory_code_links'::regclass
          AND conname = 'memory_code_links_confidence_bounds'
    ) THEN
        ALTER TABLE memory_code_links
            ADD CONSTRAINT memory_code_links_confidence_bounds
            CHECK (confidence >= 0 AND confidence <= 1)
            NOT VALID;
    END IF;
END
$guard$;

COMMIT;
