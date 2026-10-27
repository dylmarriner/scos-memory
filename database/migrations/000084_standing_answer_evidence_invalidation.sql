BEGIN;

-- P7 standing answers are never trusted after an unverified migration.
-- Existing untracked answers must be refreshed before they can be used again.
ALTER TABLE memory_standing_answers
    ADD COLUMN stale_at timestamptz,
    ADD COLUMN stale_reason text;

-- The schema owner alone may update cache rows from the evidence trigger;
-- runtime callers retain the existing tenant/namespace membership policy.
DO $owner_policy$
DECLARE owner_role name := current_user;
BEGIN
    EXECUTE format(
      'CREATE POLICY standing_cache_owner_maintenance ON memory_standing_answers FOR ALL TO %I USING (true) WITH CHECK (true)',
      owner_role
    );
END
$owner_policy$;

UPDATE memory_standing_answers
   SET stale_at = clock_timestamp(),
       stale_reason = 'unverified_legacy_evidence'
 WHERE stale_at IS NULL;

-- Typed dependencies avoid scanning/parsing arbitrary legacy JSON text on
-- every memory update and retain the exact observed supporting memory version.
CREATE TABLE memory_standing_answer_dependencies (
    standing_answer_id uuid NOT NULL REFERENCES memory_standing_answers(id) ON DELETE CASCADE,
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    fact_id uuid NOT NULL REFERENCES memory_items(id) ON DELETE CASCADE,
    observed_version_id uuid REFERENCES memory_versions(id) ON DELETE SET NULL,
    PRIMARY KEY(standing_answer_id, fact_id)
);
CREATE INDEX standing_answer_dep_fact_idx
    ON memory_standing_answer_dependencies(tenant_id, fact_id, standing_answer_id);
ALTER TABLE memory_standing_answer_dependencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE memory_standing_answer_dependencies FORCE ROW LEVEL SECURITY;

DO $dep_policy$
DECLARE owner_role name := current_user;
BEGIN
    EXECUTE format(
      'CREATE POLICY standing_dep_owner_maintenance ON memory_standing_answer_dependencies FOR ALL TO %I USING (true) WITH CHECK (true)',
      owner_role
    );
END
$dep_policy$;

REVOKE ALL ON memory_standing_answer_dependencies FROM PUBLIC;
-- scos_app may inherit broad migration-default grants; dependencies are
-- trigger-owned and must not be directly readable or rewritable.
DO $revoke_role$
BEGIN
 IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
  REVOKE ALL ON memory_standing_answer_dependencies FROM scos_app;
 END IF;
END
$revoke_role$;

-- When a cache is explicitly created/refreshed, derive its dependencies
-- from *approved, current, same-namespace evidence*. No client-provided
-- cross-tenant/superseded dependency is allowed to count as fresh.
CREATE FUNCTION sync_standing_answer_dependencies()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $sync$
DECLARE
    evidence jsonb;
    entry jsonb;
    source_fact uuid;
    observed uuid;
    already_added uuid[] := ARRAY[]::uuid[];
BEGIN
    IF NEW.stale_at IS NOT NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.evidence_fact_ids IS NULL OR btrim(NEW.evidence_fact_ids) = '' THEN
        RAISE EXCEPTION 'fresh standing answers require fact evidence'
            USING ERRCODE = '22023';
    END IF;
    BEGIN
        evidence := NEW.evidence_fact_ids::jsonb;
    EXCEPTION WHEN invalid_text_representation THEN
        RAISE EXCEPTION 'standing evidence must be a JSON array of UUIDs'
            USING ERRCODE = '22023';
    END;
    IF jsonb_typeof(evidence) <> 'array' OR jsonb_array_length(evidence) = 0 THEN
        RAISE EXCEPTION 'standing evidence must be a nonempty JSON UUID array'
            USING ERRCODE = '22023';
    END IF;
    DELETE FROM memory_standing_answer_dependencies
     WHERE standing_answer_id = NEW.id;
    FOR entry IN SELECT value FROM jsonb_array_elements(evidence) LOOP
        IF jsonb_typeof(entry) <> 'string' THEN
            RAISE EXCEPTION 'standing evidence must contain only UUID strings'
                USING ERRCODE = '22023';
        END IF;
        BEGIN
            source_fact := (entry #>> '{}')::uuid;
        EXCEPTION WHEN invalid_text_representation THEN
            RAISE EXCEPTION 'invalid standing evidence UUID'
                USING ERRCODE = '22023';
        END;
        IF source_fact = ANY(already_added) THEN
            CONTINUE;
        END IF;
        SELECT f.current_version_id INTO observed
        FROM memory_items f
        JOIN namespaces ns ON ns.id = f.namespace_id AND ns.tenant_id = f.tenant_id
        WHERE f.id = source_fact
          AND f.tenant_id = NEW.tenant_id
          AND f.namespace_id = NEW.namespace_id
          AND f.status = 'active'
          AND f.superseded_by_id IS NULL
          AND (f.expires_at IS NULL OR f.expires_at > now())
          AND (f.valid_from IS NULL OR f.valid_from <= now())
          AND (f.valid_until IS NULL OR f.valid_until > now())
          AND f.sensitivity <= ns.sensitivity_ceiling
          -- An active memory may still be unsafe to cite if its linked
          -- historical code is pending a P7 review. Only a governed
          -- still_valid reviewer decision can clear that restriction.
          AND NOT EXISTS (
              SELECT 1
              FROM memory_revalidation_events ev
              LEFT JOIN memory_revalidation_decisions d
                ON d.event_id = ev.id AND d.tenant_id = ev.tenant_id
              WHERE ev.fact_id = f.id
                AND ev.tenant_id = f.tenant_id
                AND ev.review_status IN (
                    'needs_review','superseded','invalidated','unresolved'
                )
                AND (d.outcome IS NULL OR d.outcome <> 'still_valid')
          );
        IF NOT FOUND THEN
            RAISE EXCEPTION 'standing answer references inaccessible or stale evidence'
                USING ERRCODE = '23514';
        END IF;
        INSERT INTO memory_standing_answer_dependencies
            (standing_answer_id, tenant_id, fact_id, observed_version_id)
        VALUES (NEW.id, NEW.tenant_id, source_fact, observed);
        already_added := array_append(already_added, source_fact);
    END LOOP;
    RETURN NEW;
END
$sync$;

CREATE TRIGGER standing_answer_refresh_dependencies
AFTER INSERT OR UPDATE OF evidence_fact_ids, answer, stale_at, namespace_id, tenant_id
ON memory_standing_answers
FOR EACH ROW EXECUTE FUNCTION sync_standing_answer_dependencies();

-- Transitioning a memory invalidates only the answers citing that fact.
-- Its change is in the same transaction as the governed state transition.
CREATE FUNCTION mark_dependent_standing_answers_stale()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $invalidate$
DECLARE
    reason text;
BEGIN
    IF TG_OP = 'DELETE' THEN
        reason := 'supporting_memory_deleted';
    ELSE
        IF NEW.status IS NOT DISTINCT FROM OLD.status
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
    UPDATE memory_standing_answers sa
       SET stale_at = clock_timestamp(),
           stale_reason = reason
      FROM memory_standing_answer_dependencies dep
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

CREATE TRIGGER memory_invalidate_standing_answers_on_change
AFTER UPDATE OF status,current_version_id,superseded_by_id,valid_from,valid_until,expires_at,sensitivity,namespace_id
ON memory_items FOR EACH ROW
EXECUTE FUNCTION mark_dependent_standing_answers_stale();

CREATE TRIGGER memory_invalidate_standing_answers_on_delete
BEFORE DELETE ON memory_items FOR EACH ROW
EXECUTE FUNCTION mark_dependent_standing_answers_stale();

-- A P7 code-continuity event can make a fact's supporting code stale even
-- before a reviewer changes the fact itself. Invalidate cache candidates
-- while leaving the authoritative fact and the review decision untouched.
CREATE FUNCTION mark_standing_answers_stale_for_code_review()
RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $review_stale$
BEGIN
    IF NEW.review_status IN ('needs_review','superseded','invalidated','unresolved') THEN
        UPDATE memory_standing_answers sa
           SET stale_at = clock_timestamp(),
               stale_reason = 'supporting_code_requires_review'
          FROM memory_standing_answer_dependencies dep
         WHERE dep.standing_answer_id = sa.id
           AND dep.tenant_id = NEW.tenant_id
           AND dep.fact_id = NEW.fact_id
           AND sa.stale_at IS NULL;
    END IF;
    RETURN NEW;
END
$review_stale$;

CREATE TRIGGER standing_answers_p7_code_review
AFTER INSERT ON memory_revalidation_events
FOR EACH ROW EXECUTE FUNCTION mark_standing_answers_stale_for_code_review();

COMMIT;
