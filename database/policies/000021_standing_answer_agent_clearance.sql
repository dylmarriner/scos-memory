BEGIN;

-- Fresh-install counterpart of migration 000089. Reinstall transactionally
-- if the migration already applied it on an upgrade with an existing role.
DROP POLICY IF EXISTS standing_cache_runtime_principal_clearance
ON public.memory_standing_answers;
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
);

COMMIT;
