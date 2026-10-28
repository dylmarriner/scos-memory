BEGIN;
-- P7: source text is evidence too. A direct SQL UPDATE that changes the
-- canonical summary or exact-title lookup key must revoke a standing answer
-- even if no version/status column changes. Previously that update could
-- leave an old answer marked fresh and readable indefinitely.
-- The migration locks both affected tables while replacing the trigger and
-- invalidating pre-upgrade cache rows whose text drift cannot be audited.
LOCK TABLE memory_items, memory_standing_answers IN SHARE ROW EXCLUSIVE MODE;

CREATE OR REPLACE FUNCTION mark_dependent_standing_answers_stale()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $invalidate$
DECLARE
    reason text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        reason := 'supporting_memory_deleted';
    ELSE
        IF NEW.title IS NOT DISTINCT FROM OLD.title
           AND NEW.canonical_summary IS NOT DISTINCT FROM OLD.canonical_summary
           AND NEW.status IS NOT DISTINCT FROM OLD.status
           AND NEW.current_version_id IS NOT DISTINCT FROM OLD.current_version_id
           AND NEW.superseded_by_id IS NOT DISTINCT FROM OLD.superseded_by_id
           AND NEW.valid_from IS NOT DISTINCT FROM OLD.valid_from
           AND NEW.valid_until IS NOT DISTINCT FROM OLD.valid_until
           AND NEW.expires_at IS NOT DISTINCT FROM OLD.expires_at
           AND NEW.sensitivity IS NOT DISTINCT FROM OLD.sensitivity
           AND NEW.namespace_id IS NOT DISTINCT FROM OLD.namespace_id THEN
            RETURN NEW;
        END IF;
        reason := 'supporting_memory_changed';
    END IF;
    UPDATE public.memory_standing_answers sa
       SET stale_at = clock_timestamp(),
           stale_reason = reason
      FROM public.memory_standing_answer_dependencies dep
     WHERE dep.standing_answer_id = sa.id
       AND dep.tenant_id = OLD.tenant_id
       AND dep.fact_id = OLD.id
       AND sa.stale_at IS NULL;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$invalidate$;


DROP TRIGGER memory_invalidate_standing_answers_on_change ON memory_items;
CREATE TRIGGER memory_invalidate_standing_answers_on_change
AFTER UPDATE OF title,canonical_summary,status,current_version_id,
    superseded_by_id,valid_from,valid_until,expires_at,sensitivity,namespace_id
ON memory_items FOR EACH ROW
EXECUTE FUNCTION mark_dependent_standing_answers_stale();

-- Existing answers might already have drifted without an event. Do not
-- grandfather unverifiable cached prose into the stronger freshness contract.
UPDATE memory_standing_answers
   SET stale_at=clock_timestamp(), stale_reason='source_text_guard_upgrade'
 WHERE stale_at IS NULL;
COMMIT;
