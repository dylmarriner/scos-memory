BEGIN;

-- P7: runtime SQL writers may only materialize the deterministic, governed
-- exact-title/summary cache. Arbitrary legacy/curated cache writes must be
-- performed by the schema owner after independent governance approval.
-- Existing unverified caches are invalidated; this migration does not claim
-- to authenticate the underlying source memory's original creation.
-- Serialize old writers with trigger installation and legacy invalidation.
LOCK TABLE memory_standing_answers IN SHARE ROW EXCLUSIVE MODE;
CREATE FUNCTION guard_generated_standing_answer_write()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $guard$
DECLARE
    evidence jsonb;
    source_id uuid;
    source_title text;
    source_summary text;
    source_confidence double precision;
BEGIN
    -- The migration owner may curate answers; ordinary runtime SQL writers
    -- cannot disguise a previously generated answer as a legacy/curated row.
    IF current_user <> 'scos_app' THEN
        RETURN NEW;
    END IF;
    -- Only the owner may author or change curated/legacy rows, even while
    -- stale. Otherwise a restricted writer could squat on a question key
    -- with a stale legacy INSERT and block the governed refresh endpoint.
    -- Trigger-owned invalidations execute as the migration owner.
    IF (TG_OP = 'UPDATE'
        AND OLD.refresh_reason IS DISTINCT FROM 'verified_fact_title_summary')
       OR NEW.refresh_reason IS DISTINCT FROM 'verified_fact_title_summary'
       OR NEW.stale_at IS NOT NULL THEN
        RAISE EXCEPTION 'runtime standing answers require governed generation'
            USING ERRCODE = '23514';
    END IF;
    BEGIN
        evidence := NEW.evidence_fact_ids::jsonb;
        IF jsonb_typeof(evidence) IS DISTINCT FROM 'array'
           OR jsonb_array_length(evidence) <> 1
           OR jsonb_typeof(evidence -> 0) IS DISTINCT FROM 'string' THEN
            RAISE EXCEPTION 'generated standing answer requires exactly one source'
                USING ERRCODE = '23514';
        END IF;
        source_id := (evidence ->> 0)::uuid;
    EXCEPTION WHEN invalid_text_representation THEN
        RAISE EXCEPTION 'generated standing answer has invalid source evidence'
            USING ERRCODE = '23514';
    END;
    SELECT f.title, f.canonical_summary, f.confidence_score
      INTO source_title, source_summary, source_confidence
    FROM public.memory_items f
    WHERE f.id = source_id AND f.tenant_id = NEW.tenant_id
      AND f.namespace_id = NEW.namespace_id
    FOR SHARE OF f;
    IF NOT FOUND
       OR NEW.question_key IS DISTINCT FROM lower(btrim(source_title))
       OR NEW.answer IS DISTINCT FROM source_summary
       OR NEW.confidence IS DISTINCT FROM source_confidence
       OR NEW.stale_reason IS NOT NULL THEN
        RAISE EXCEPTION 'generated standing answer differs from governed source'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END
$guard$;

REVOKE ALL ON FUNCTION guard_generated_standing_answer_write() FROM PUBLIC;
-- Runtime never needs to delete a cache row. Do not let an authenticated
-- candidate writer remove someone else's curated/verified answer.
DO $runtime_privileges$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        REVOKE DELETE, TRUNCATE ON memory_standing_answers FROM scos_app;
    END IF;
END
$runtime_privileges$;
CREATE TRIGGER generated_standing_answer_write_integrity
BEFORE INSERT OR UPDATE ON memory_standing_answers
FOR EACH ROW EXECUTE FUNCTION guard_generated_standing_answer_write();

-- Any earlier fresh cache may have been written or altered directly under
-- the former broad runtime grant. No prior answer text is grandfathered.
UPDATE memory_standing_answers
   SET stale_at = clock_timestamp(),
       stale_reason = 'runtime_cache_write_guard_upgrade'
 WHERE stale_at IS NULL;

COMMIT;
