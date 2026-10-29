BEGIN;

-- Fresh-install counterpart of migration 000090: 000011 creates scos_app
-- after migrations run. Only the source-derived SECURITY DEFINER refresh
-- entry point may see stale conflict rows; 000021's restrictive SELECT policy
-- stays intact and is never relaxed for direct runtime reads.
REVOKE ALL ON FUNCTION public.scos_refresh_verified_standing_answer(uuid,text)
    FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.scos_refresh_verified_standing_answer(uuid,text)
    TO scos_app;

COMMIT;
