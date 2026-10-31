# SCOS PostgreSQL DNS diagnosis (read-only)

**Purpose:** Investigate the unresolved production verification issue
[#181](https://github.com/dylmarriner/scos-memory/issues/181) without
restarting containers, modifying deployment state, reading credentials, or
treating stale prefetch failures as proof of current health.

## Preconditions

1. Verify the production host identity and SSH host key using an independently
   trusted management channel. The September 2026
   [production-access snapshot](production-access.md) is not current proof.
2. Run only on that verified Docker host as an operator authorized to read
   Docker metadata. Docker access is highly privileged; do not expose it
   to untrusted users or give this tool a public endpoint.
3. Identify the Compose **project label** by inspecting your authorized
   deployment configuration. The historical deployed project was `compose`;
   do not assume it still is.
4. No `.env` contents, credentials, client bearer tokens, or production
   database connectivity URLs are required.

## Diagnostic commands

From the deployed repository checkout:

```bash
python3 scripts/postgres_dns_doctor.py --project compose --dns-probe
```

Use `--project NAME` for a different project. Omit `--dns-probe` for
metadata-only inspection; this intentionally returns **indeterminate** when
the network wiring looks valid but name resolution was not tested.

The script runs only `docker ps --all`, `docker inspect`, and, when
explicitly requested and wiring is viable,
`docker exec <hermes-provider-id> getent hosts postgres`.
It never calls `docker compose up/down`, `docker restart`, writes state,
or reads `DATABASE_URL`. No raw inspect JSON, environment variables,
container IDs, DNS output, or Docker stderr enter the diagnostic report.

Exit codes: **0** = Docker network and opt-in DNS resolution pass,
**2** = a known network/DNS failure, **3** = inconclusive (including Docker
inspection errors or not requesting the DNS probe). Exit 0 is **not**
PostgreSQL login, tenant RLS, memory write, recovery, or rollout acceptance.

Inspect `network_result`, `dns_probe`, `services`, and `issues`.
Typical failures:

- `postgres_missing` or `hermes_provider_missing`: expected container
  absent from the requested project.
- `no_shared_network`: the application and database containers cannot
  resolve one another on a common Compose network.
- `postgres_alias_missing`: the database lacks the `postgres` name on
  networks shared with the provider.
- `postgres_dns_probe_failed`: the application container could not resolve
  `postgres` via `getent`; this can also indicate missing `getent`
  support. Verify separately before attributing it to Docker DNS.
- `postgres_unhealthy`: database container is reported unhealthy by Docker.
  This alone does not prove the failure is DNS rather than database startup.

## Completion gates

Record a timestamped, redacted diagnosis, deployed source SHA and host identity.
Before closing issue #181, separately confirm real authenticated PostgreSQL
readiness, a tenant-scoped memory write/read round-trip, backup/restore evidence,
and a controlled recovery drill. Do not run a production deployment, modify
network settings, restart containers or perform data migrations without the
owner's explicit approval.

This tool is a source-level operations aid. It is **not** evidence that
the live deployment has been accessed or repaired.
