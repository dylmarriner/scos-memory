BEGIN;

-- P7 read-time freshness is separate from trigger invalidation: a fact can
-- expire solely because time advances, without any row UPDATE. This view is
-- a fail-closed read primitive, not an answer generator or a cache-serving
-- endpoint. It revalidates every cited fact on every read.
--
-- SECURITY INVOKER is essential: the caller's FORCE-RLS policies apply to
-- both the cache and the supporting memories/revalidation evidence. Never
-- expose this projection through the migration owner's privileges.
CREATE VIEW public.memory_standing_answers_current
WITH (security_invoker = true, security_barrier = true)
AS
SELECT sa.*
FROM public.memory_standing_answers sa
WHERE CASE
    -- The CASE also prevents parsing untrusted legacy evidence text. Every
    -- pre-000084 row is stale until a validated refresh occurs.
    WHEN sa.stale_at IS NOT NULL
      OR sa.evidence_fact_ids IS NULL
      OR btrim(sa.evidence_fact_ids) = '' THEN false
    ELSE
      jsonb_typeof(sa.evidence_fact_ids::jsonb) = 'array'
      AND jsonb_array_length(sa.evidence_fact_ids::jsonb) > 0
      AND NOT EXISTS (
          SELECT 1
          FROM jsonb_array_elements_text(sa.evidence_fact_ids::jsonb)
               AS cited(fact_id)
          WHERE NOT EXISTS (
              SELECT 1
              FROM public.memory_items f
              JOIN public.namespaces ns
                ON ns.id = f.namespace_id AND ns.tenant_id = f.tenant_id
              WHERE f.id = cited.fact_id::uuid
                AND f.tenant_id = sa.tenant_id
                AND f.namespace_id = sa.namespace_id
                AND f.status = 'active'
                AND f.superseded_by_id IS NULL
                AND (f.expires_at IS NULL OR f.expires_at > now())
                AND (f.valid_from IS NULL OR f.valid_from <= now())
                AND (f.valid_until IS NULL OR f.valid_until > now())
                AND f.sensitivity <= ns.sensitivity_ceiling
                AND NOT EXISTS (
                    SELECT 1
                    FROM public.memory_revalidation_events ev
                    LEFT JOIN public.memory_revalidation_decisions d
                      ON d.event_id = ev.id AND d.tenant_id = ev.tenant_id
                    WHERE ev.fact_id = f.id
                      AND ev.tenant_id = f.tenant_id
                      AND ev.review_status IN (
                          'needs_review','superseded','invalidated','unresolved'
                      )
                      AND (d.outcome IS NULL OR d.outcome <> 'still_valid')
                )
          )
      )
END;

-- Only read access is needed. Existing table and source RLS remain mandatory.
REVOKE ALL ON public.memory_standing_answers_current FROM PUBLIC;
DO $runtime_grant$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
    GRANT SELECT ON public.memory_standing_answers_current TO scos_app;
  END IF;
END
$runtime_grant$;

COMMIT;
