BEGIN;

-- agent-registry's staleness sweep (migrations/000043) is the one
-- cross-tenant operation scos_app may perform; no other role may run it.
REVOKE EXECUTE ON FUNCTION agent_registrations_expire_stale() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION agent_registrations_expire_stale() TO scos_app;

COMMIT;
