BEGIN;

-- P7: a restrictive SELECT policy correctly hides stale/over-clearance cache
-- rows from scos_app, but INSERT ... ON CONFLICT DO UPDATE must see the
-- conflicting row. Never relax raw SELECT to repair this. Instead expose one
-- narrowly authorized, source-derived write operation through the table owner.
-- It takes no caller-supplied answer, evidence, confidence, or namespace.
CREATE FUNCTION public.scos_refresh_verified_standing_answer(
    requested_fact_id uuid, requested_question_key text
) RETURNS uuid
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $refresh$
DECLARE
    actor_tenant uuid;
    actor_principal uuid;
    source_row record;
    cache_id uuid;
    normalized_title text;
    evidence_text text;
BEGIN
    actor_tenant := NULLIF(current_setting('scos.tenant_id', true), '')::uuid;
    actor_principal := NULLIF(current_setting('scos.principal_id', true), '')::uuid;
    IF actor_tenant IS NULL OR actor_principal IS NULL OR requested_fact_id IS NULL
       OR requested_question_key IS NULL THEN
        RETURN NULL;
    END IF;

    -- Re-authorize inside the definer, not just in the HTTP handler: scos_app
    -- can call this function directly. Pin the source under a SHARE lock for
    -- the full caller transaction so invalidation cannot race the refresh.
    SELECT f.namespace_id, f.title, f.canonical_summary, f.confidence_score
      INTO source_row
      FROM public.memory_items f
      JOIN public.namespaces ns
        ON ns.id = f.namespace_id AND ns.tenant_id = f.tenant_id
      JOIN public.namespace_memberships m
        ON m.namespace_id = f.namespace_id AND m.principal_id = actor_principal
      JOIN public.principals p
        ON p.id = actor_principal AND p.tenant_id = f.tenant_id
      LEFT JOIN public.agents a ON a.id = p.id
     WHERE f.id = requested_fact_id
       AND f.tenant_id = actor_tenant
       AND p.disabled_at IS NULL
       AND m.can_read AND m.can_write_candidates
       AND (p.kind <> 'agent'
            OR (a.id IS NOT NULL AND f.sensitivity <= a.maximum_sensitivity))
       AND f.status = 'active' AND f.superseded_by_id IS NULL
       AND f.sensitivity <= ns.sensitivity_ceiling
       AND (f.expires_at IS NULL OR f.expires_at > statement_timestamp())
       AND (f.valid_from IS NULL OR f.valid_from <= statement_timestamp())
       AND (f.valid_until IS NULL OR f.valid_until > statement_timestamp())
     FOR SHARE OF f;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    -- An authorized source whose code continuity is still under review is
    -- ineligible, not a cache-key conflict. Preserve the existing HTTP 409
    -- evidence-stale contract by returning the exact integrity SQLSTATE.
    -- Unauthorized or invisible sources above still return NULL, avoiding
    -- disclosure of whether a protected fact exists.
    IF EXISTS (
        SELECT 1
        FROM public.memory_revalidation_events ev
        LEFT JOIN public.memory_revalidation_decisions d
          ON d.event_id = ev.id AND d.tenant_id = ev.tenant_id
        WHERE ev.fact_id = requested_fact_id AND ev.tenant_id = actor_tenant
          AND ev.review_status IN
              ('needs_review','superseded','invalidated','unresolved')
          AND (d.outcome IS NULL OR d.outcome <> 'still_valid')
    ) THEN
        RAISE EXCEPTION 'standing answer references inaccessible or stale evidence'
            USING ERRCODE = '23514';
    END IF;

    -- Match the generated-cache integrity trigger's title normalization.
    normalized_title := lower(btrim(source_row.title,
        E' \t\n\r\f\v' || U&'\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000'));
    IF requested_question_key IS DISTINCT FROM normalized_title
       OR octet_length(requested_question_key) = 0
       OR octet_length(requested_question_key) > 1024
       OR source_row.canonical_summary IS NULL
       OR btrim(source_row.canonical_summary) = ''
       OR octet_length(source_row.canonical_summary) > 16384 THEN
        RETURN NULL;
    END IF;

    evidence_text := jsonb_build_array(requested_fact_id)::text;
    INSERT INTO public.memory_standing_answers (
        tenant_id, namespace_id, question_key, answer, evidence_fact_ids,
        confidence, last_refreshed_at, refresh_reason, stale_at, stale_reason
    ) VALUES (
        actor_tenant, source_row.namespace_id, requested_question_key,
        source_row.canonical_summary, evidence_text, source_row.confidence_score,
        statement_timestamp(), 'verified_fact_title_summary', NULL, NULL
    )
    ON CONFLICT (tenant_id, namespace_id, question_key) DO UPDATE SET
        answer = EXCLUDED.answer,
        evidence_fact_ids = EXCLUDED.evidence_fact_ids,
        confidence = EXCLUDED.confidence,
        last_refreshed_at = EXCLUDED.last_refreshed_at,
        refresh_reason = EXCLUDED.refresh_reason,
        stale_at = NULL, stale_reason = NULL
    WHERE memory_standing_answers.refresh_reason = 'verified_fact_title_summary'
      AND (
          memory_standing_answers.evidence_fact_ids = EXCLUDED.evidence_fact_ids
          OR memory_standing_answers.stale_at IS NOT NULL
          OR EXISTS (
              SELECT 1 FROM public.memory_items old_source
              WHERE old_source.tenant_id = memory_standing_answers.tenant_id
                AND old_source.namespace_id = memory_standing_answers.namespace_id
                AND memory_standing_answers.evidence_fact_ids =
                    ('["' || old_source.id::text || '"]')
                AND (old_source.expires_at <= statement_timestamp()
                     OR old_source.valid_until <= statement_timestamp())
          )
      )
    RETURNING id INTO cache_id;

    IF cache_id IS NULL THEN
        RETURN NULL;
    END IF;
    -- The owner executes this view with the caller's authenticated GUCs.
    -- Refuse to commit a generated answer that the read-time freshness and
    -- clearance gates would not serve, even when called directly from SQL.
    IF NOT EXISTS (
        SELECT 1 FROM public.memory_standing_answers_current v
        WHERE v.id = cache_id AND v.tenant_id = actor_tenant
    ) THEN
        RAISE EXCEPTION 'standing answer references inaccessible or stale evidence'
            USING ERRCODE = '23514';
    END IF;
    RETURN cache_id;
END
$refresh$;

REVOKE ALL ON FUNCTION public.scos_refresh_verified_standing_answer(uuid,text)
    FROM PUBLIC;
-- Upgrade installations already have scos_app; fresh installations create
-- that role in policies/000011 and grant in policies/000022.
DO $runtime_grant$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        GRANT EXECUTE ON FUNCTION public.scos_refresh_verified_standing_answer(uuid,text)
            TO scos_app;
    END IF;
END
$runtime_grant$;

COMMIT;
