BEGIN;

-- P7: a cached answer is sensitive evidence even when its text has already
-- been materialized. The existing security-invoker view checked namespace
-- membership and source freshness but did not enforce an agent's independent
-- maximum_sensitivity or principal lifecycle. A valid namespace membership
-- is not an authorization to exceed the agent capability ceiling.
--
-- Preserve the security-invoker/security-barrier boundary and the CASE guard
-- for malformed legacy evidence_fact_ids. Do not change the cache contents:
-- this is a read-time authorization fix, not a data migration.
CREATE OR REPLACE VIEW public.memory_standing_answers_current
WITH (security_invoker = true, security_barrier = true)
AS
SELECT sa.*
FROM public.memory_standing_answers sa
WHERE CASE
    WHEN sa.stale_at IS NOT NULL
      OR sa.evidence_fact_ids IS NULL
      OR btrim(sa.evidence_fact_ids) = '' THEN false
    ELSE
      jsonb_typeof(sa.evidence_fact_ids::jsonb) = 'array'
      AND jsonb_array_length(sa.evidence_fact_ids::jsonb) > 0
      AND EXISTS (
          SELECT 1
          FROM public.principals p
          LEFT JOIN public.agents a ON a.id = p.id
          WHERE p.id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
            AND p.tenant_id = sa.tenant_id
            AND p.disabled_at IS NULL
            AND (p.kind <> 'agent' OR a.id IS NOT NULL)
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
                      AND (f.expires_at IS NULL OR f.expires_at > statement_timestamp())
                      AND (f.valid_from IS NULL OR f.valid_from <= statement_timestamp())
                      AND (f.valid_until IS NULL OR f.valid_until > statement_timestamp())
                      AND f.sensitivity <= ns.sensitivity_ceiling
                      AND (p.kind <> 'agent' OR f.sensitivity <= a.maximum_sensitivity)
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
      )
END;

REVOKE ALL ON public.memory_standing_answers_current FROM PUBLIC;
DO $runtime_grant$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
    GRANT SELECT ON public.memory_standing_answers_current TO scos_app;
  END IF;
END
$runtime_grant$;

-- Existing deployments already have scos_app when this migration runs.
-- Fresh installations create that role later in policies/000011; the matching
-- 000021 policy file installs this same restrictive rule after role creation.
-- Restrict direct table SELECT too: otherwise scos_app could bypass the view
-- and read a cached answer without any agent capability check.
DO $policy$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'scos_app') THEN
        EXECUTE $ddl$
CREATE POLICY standing_cache_runtime_principal_clearance
ON public.memory_standing_answers AS RESTRICTIVE
FOR SELECT TO scos_app
USING (
    EXISTS (
        SELECT 1
        FROM public.principals p
        LEFT JOIN public.agents a ON a.id = p.id
        WHERE p.id = NULLIF(current_setting('scos.principal_id', true), '')::uuid
          AND p.tenant_id = memory_standing_answers.tenant_id
          AND p.disabled_at IS NULL
          AND (
              p.kind <> 'agent'
              OR (
                  a.id IS NOT NULL
                  AND CASE
                      WHEN memory_standing_answers.stale_at IS NOT NULL
                        OR memory_standing_answers.evidence_fact_ids IS NULL
                        OR btrim(memory_standing_answers.evidence_fact_ids) = ''
                      THEN false
                      ELSE
                          jsonb_typeof(memory_standing_answers.evidence_fact_ids::jsonb) = 'array'
                          AND jsonb_array_length(memory_standing_answers.evidence_fact_ids::jsonb) > 0
                          AND NOT EXISTS (
                              SELECT 1
                              FROM jsonb_array_elements_text(memory_standing_answers.evidence_fact_ids::jsonb)
                                   AS cited(fact_id)
                              WHERE NOT EXISTS (
                                  SELECT 1 FROM public.memory_items f
                                  WHERE f.id = cited.fact_id::uuid
                                    AND f.tenant_id = memory_standing_answers.tenant_id
                                    AND f.namespace_id = memory_standing_answers.namespace_id
                                    AND f.sensitivity <= a.maximum_sensitivity
                              )
                          )
                  END
              )
          )
    )
)$ddl$;
    END IF;
END
$policy$;

COMMIT;
