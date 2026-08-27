# Specialist audit artifact

```text
Skill: hostile-error-path-auditor
Agent: hostile-error-path-auditor-workflow-agent
Scope: services/backup-manager, services/recovery-orchestrator, services/security-gate (secret resolution),
  database/migrations + database/policies (rollback / RLS enforcement), packages/auth/middleware.go
  (shared auth fail-open surface reached from the above), deploy/native + deploy/compose (how those
  services and the DB role are actually provisioned in production).

Files/components inspected:
  - services/backup-manager/cmd/server/main.go (full, 1217 lines)
  - services/recovery-orchestrator/cmd/server/main.go (full, 564 lines)
  - services/security-gate/cmd/server/main.go (full, 917 lines)
  - services/security-gate/cmd/server/store.go (full, 364 lines)
  - packages/auth/middleware.go (full, 231 lines)
  - database/migrations/000001_initial.sql .. 000020_dream_merge_synthesis.sql (all 20 files, line counts
    and BEGIN/COMMIT wrapping enumerated for every file)
  - database/policies/000001_rls.sql .. 000009_hierarchy_nodes_rls.sql, database/policies/README.md
  - database/integration/rls_test.go (role/fixture setup, lines ~180-240)
  - scripts/scos-migrate (full)
  - deploy/native/install.sh, deploy/native/postgres-setup.sh
  - deploy/compose/docker-compose.yml (env wiring for DATABASE_URL/SCOS_DATABASE_URL across all services)

Entry points inspected:
  - backup-manager HTTP: /v1/backups/start, /v1/backups/complete, /v1/backups/{id}/restore,
    /v1/restore-tests/*, /v1/restore-tests/cadence (+ background runRestoreCadenceScheduler)
  - recovery-orchestrator HTTP: /v1/recovery/start, /v1/recovery/checklist, /v1/recovery/complete,
    /v1/recovery/status[/​{id}], /v1/recovery/interrupted-turns
  - security-gate HTTP: /v1/auth/issue|validate|revoke, /v1/permissions/check|classify,
    /v1/audit, /v1/audit/{id}, /v1/audit/record, /v1/secrets, /v1/secrets/{id}, /v1/secrets/{id}/resolve
  - scripts/scos-migrate (deployment-time entry point, invoked by deploy/native/install.sh and operators)
  - packages/auth.AuthenticatedHandler / validateClaims (shared entry gate in front of every service above)

Important callers/callees:
  - backup-manager: handleProductionBackupStart -> pg_dump (exec) -> encryptBackup(AES-GCM) ->
    writeAtomic(artifact) -> writeAtomic(manifest); restoreProduction -> createdb -> pg_restore -> deferred dropdb
  - recovery-orchestrator: handleRecoveryStart -> discoverRecoveryFindings (turns/operations/outbox/okf_bundles/
    checkpoints/recovery_leases queries) -> persistRecoveryStatus (recovery_runs/recovery_leases/recovery_events)
  - security-gate: handleSecretResolve -> getSecretReference (RLS-scoped read) -> resolveSecretValue ->
    securestoreClient.getSecret (external HTTP)
  - packages/auth.AuthenticatedHandler -> validateClaims (HMAC-verified claims token) or shared-token compare
  - scripts/scos-migrate -> psql -f <file> (per-file transaction) then a *separate* psql -c INSERT into
    scos_schema_migrations
  - Every RLS policy in database/policies/*.sql -> current_setting('scos.tenant_id'/'scos.principal_id', true)
    set by each service's setPrincipalContext-equivalent inside its own transaction

Workflows traced:
  1. database migration apply/rollback: scos-migrate loop, per-file transaction boundaries, tracking-table
     bookkeeping, and the complete absence of down-migrations.
  2. RLS tenant isolation as actually enforced against the database role the services connect with in
     the shipped deployment tooling (native install + docker-compose), vs. as enforced in the integration
     test fixture.
  3. backup-manager production backup creation failure/crash paths (pg_dump -> plaintext temp file ->
     encryption -> atomic write) and restore-drill cleanup (createdb/pg_restore/dropdb).
  4. recovery-orchestrator recovery-start / retry path, including the recovery_leases upsert on failure/retry.
  5. security-gate secret resolution fail-closed behavior (approval_required, unreachable upstream, missing
     config) and the bearer-claims validation path shared by every service via packages/auth.

Trust boundaries:
  - Internal service-to-service calls (SCOS_INTERNAL_SERVICE_TOKEN / per-request JWT-like claims token,
    packages/auth/middleware.go) in front of backup-manager, recovery-orchestrator, security-gate.
  - security-gate's own bearer-token layer (HMAC-signed public-id.signature) in front of permission/audit/
    secret endpoints, independent of the internal service token.
  - security-gate -> Securestore REST API (external secret-manager boundary), authenticated with a static
    agent API key.
  - Postgres RLS boundary: application-set `scos.tenant_id`/`scos.principal_id` session GUCs vs. the
    connecting database role's actual privileges (this is the boundary found to be broken, see Findings #1).
  - Operator/deploy-time boundary: whoever runs scos-migrate against production controls schema state,
    including recovery from failure.

Attacker-controlled inputs:
  - HTTP request bodies/headers to all listed endpoints (JSON bodies, Authorization header, X-SCOS-* headers).
  - `id` path segments on /v1/backups/{id}/restore, /v1/secrets/{id}, /v1/secrets/{id}/resolve,
    /v1/audit/{id}, /v1/recovery/status/{id} (mitigated for path traversal by Go's http.ServeMux path
    cleaning, verified by reading net/http's documented behavior — not independently re-tested here).
  - Any tenant/principal claim an attacker could get signed if the issuer secret is ever compromised,
    specifically one omitting/zeroing "exp" (packages/auth/middleware.go).
  - The DATABASE_URL role privileges are not attacker-controlled per se, but the finding below shows the
    *design* fails to constrain a normally-privileged application role from bypassing tenant RLS.

Invariants tested:
  - "A migration that fails or is interrupted leaves the database in a state from which scos-migrate can
    resume automatically." -> VIOLATED (Finding #2).
  - "Every table with a tenant_id RLS policy is inaccessible to a session that has not set scos.tenant_id
    to the caller's own tenant." -> VIOLATED for the database role the shipped deployment tooling actually
    provisions and wires into every service's DATABASE_URL (Finding #1).
  - "A crashed backup operation leaves no live plaintext copy of the database on disk." -> VIOLATED in the
    crash-before-cleanup window (Finding #3).
  - "A bearer claims token is only valid until its exp claim." -> VIOLATED when exp is absent/zero
    (Finding #4).
  - "A failed/retried recovery-start cannot silently take over an in-progress recovery's mutual-exclusion
    lease." -> VIOLATED (Finding #5).
  - security-gate secret resolution fails closed on every error path (missing config, upstream unreachable,
    approval_required, non-2xx) -> HELD (no bypass found; Non-issue, listed under Suspicions for completeness).

Findings:
  1. [Critical] RLS tenant isolation is not actually enforced against the database role the shipped
     deployment tooling provisions — `FORCE ROW LEVEL SECURITY` is applied on zero tables across the
     entire schema (`grep -rn "FORCE ROW LEVEL SECURITY" database/` = no matches), and
     deploy/native/postgres-setup.sh creates exactly one Postgres role (`POSTGRES_USER`, default `scos`)
     which both owns the database (`CREATE DATABASE ... OWNER $POSTGRES_USER`) and is the role every
     service's DATABASE_URL/SCOS_DATABASE_URL connects as (same postgres-setup.sh, and every service block
     in deploy/compose/docker-compose.yml requires `SCOS_DATABASE_URL` with no separate app-role variable).
     By documented PostgreSQL semantics, `ENABLE ROW LEVEL SECURITY` alone does not apply to the table's
     owner; only `FORCE ROW LEVEL SECURITY` does. Since migrations run as (and every table is therefore
     owned by) the same role every service later connects as, every `set_config('scos.tenant_id', ...)`
     call throughout the codebase (security-gate store.go, recovery-orchestrator persistRecoveryStatus,
     and every other service using this pattern) is decorative: the connecting role can read/write every
     tenant's rows regardless of what GUC is set, or even with no GUC set at all.
     database/policies/README.md itself documents the *intended* design ("before normal application roles
     are granted access" — implying a distinct, restricted role), but no script in this repository ever
     creates that role. The only place a correctly-scoped non-owner role is created is the integration
     test fixture, database/integration/rls_test.go:212 (`CREATE ROLE ... NOSUPERUSER NOBYPASSRLS`) — so
     the test suite proves RLS *can* work, while the deployment tooling never configures it to actually
     work in production. This is exactly the "insecure fail-open behavior... auth/tenant checks" pattern
     called out in scope: every RLS-gated table fails open for the entire fleet under the tooling this repo
     ships.
     Evidence: `grep -rn "FORCE ROW LEVEL SECURITY" database/` (no output); deploy/native/postgres-setup.sh
     lines ~19,69-76,122-123; deploy/compose/docker-compose.yml lines 191,311-988 (repeated
     `SCOS_DATABASE_URL`/`DATABASE_URL: ${SCOS_DATABASE_URL...}`); database/policies/README.md lines 3-5;
     database/integration/rls_test.go lines 192-238.
     Confidence: Confirmed (code + config reading; Postgres RLS-owner-bypass semantics are documented
     upstream behavior, not itself re-verified against a live cluster in this pass — see Coverage gaps).

  2. [Medium] database/migrations has no rollback ("down") migrations at all (`grep -l -i
     "down\|rollback" database/migrations/*.sql` = empty across all 20 files), and none of the CREATE
     TABLE/CREATE INDEX/ALTER TABLE statements use IF NOT EXISTS (verified via grep across all files).
     scripts/scos-migrate applies each file's DDL in its own transaction (every file is individually
     wrapped in BEGIN;...COMMIT;, confirmed for all 20 files) but then records that file as applied with a
     **separate, non-transactional** `psql -c "INSERT INTO scos_schema_migrations..."` call. If the
     migration process is interrupted (killed, network drop, host crash) after a file's COMMIT but before
     that follow-up INSERT commits, the bookkeeping table under-reports what has actually been applied.
     Because the DDL is not idempotent, re-running scos-migrate (`set -eu`, so any psql failure aborts the
     whole script) then re-attempts that already-applied file and fails with "relation already exists",
     which blocks every subsequent migration and requires a human to manually reconcile
     scos_schema_migrations before deploys can proceed again. Combined with the complete absence of down
     migrations, there is no automated path to either resume cleanly from this state or roll a bad/partial
     migration back.
     Evidence: scripts/scos-migrate (full file, especially the per-file loop at the bottom); grep results
     for `^BEGIN;\|^COMMIT;` across database/migrations/*.sql (all 20 files individually transactional);
     grep for `CREATE TABLE|CREATE INDEX|ALTER TABLE` without `IF NOT EXISTS` (all matches lack it).
     Confidence: Confirmed (direct code reading of the runner and every migration file).

  3. [Medium] services/backup-manager's production backup path writes the entire plaintext pg_dump output
     to an OS temp file before it is encrypted. `handleProductionBackupStart` (main.go:855-924) calls
     `os.CreateTemp("", "scos-pg-dump-*")` (system temp dir, not the encrypted backupDir), runs
     `pg_dump --file <plainPath>` into it, reads it back, encrypts it, and only then removes the plaintext
     via `defer os.Remove(plainPath)` (line 887). That defer only runs if the Go process is still alive to
     execute it. A crash of the process (OOM kill, SIGKILL, host power loss) at any point between the
     pg_dump completing and the defer firing leaves a complete, unencrypted database dump — including
     tenant data and any material referenced by secret_references — sitting in the shared system temp
     directory indefinitely; there is no startup-time sweep in newBackupServer/newBackupServerWithDir that
     looks for and removes orphaned `scos-pg-dump-*` files from a prior crashed run.
     Evidence: services/backup-manager/cmd/server/main.go lines 879-887 (CreateTemp + pg_dump + defer
     Remove), lines 139-162 (newBackupServer/newBackupServerWithDir — no temp-dir sweep on startup).
     Confidence: High-confidence (the code path and crash window are directly verifiable; likelihood/impact
     depend on host-level access an attacker would additionally need, which is why this is Medium not
     Critical — see Coverage gaps for what would raise confidence further).

  4. [Low] packages/auth/middleware.go's validateClaims does not enforce expiration when the "exp" claim
     is absent or zero: `if raw.Exp > 0 && !time.Now().UTC().Before(time.Unix(raw.Exp, 0)) { return
     ..."expired bearer claims" }` (line 213) is skipped entirely when raw.Exp == 0, so a claims token
     without an exp field validates forever (the `expiresAt` computed afterward at line 220-223 is only
     metadata returned in Claims.ExpiresAt; it is never itself re-checked). Every current minter found in
     this repo (services/transparency-projection/cmd/server/main.go lines 423, 546) does set "exp", so this
     is not reachable through the code paths inspected here, but it is a latent fail-open default in a
     shared library reused by every service in this repo's trust boundary: any future or misconfigured
     issuer that omits "exp" (or a forged token if the issuer secret is ever compromised, since exp=0
     requires no extra effort from an attacker who already has the secret) produces a token that never
     expires, contradicting the "revocation and expiry" invariant implied by the token design.
     Evidence: packages/auth/middleware.go lines 173-226; grep for `"exp"` / `IssuerSecret` shows the only
     two current minters both set exp.
     Confidence: Plausible / hardening opportunity (no exploitable path found in this repo's current
     minters, but the shared library itself fails open on this input).

  5. [Low] services/recovery-orchestrator's recovery-lease acquisition can silently steal an active lease.
     persistRecoveryStatus (main.go:443-511), on the first persist for a new RecoveryStatus (RecoveryRunID
     == ""), does `INSERT INTO recovery_leases (...) VALUES (...) ON CONFLICT (lease_key) DO UPDATE SET
     ... state='active', acquired_at=now(), expires_at=EXCLUDED.expires_at, released_at=NULL` (lines
     467-471) unconditionally — it never checks whether the existing row's state is already 'active' and
     unexpired for a *different* recovery_run_id before overwriting it. Since handleRecoveryStart
     (line 143) always builds a brand-new RecoveryStatus with RecoveryRunID == "" on every call, a second
     POST to /v1/recovery/start for the same service_id while a first recovery is genuinely still in
     progress creates a second recovery_runs row and overwrites the lease to point at the new run, without
     any error path back to the caller. This defeats the mutual-exclusion purpose the lease exists for
     (see the discoverRecoveryFindings step 8 comment on abandoned-lease reset), though the blast radius is
     limited to recovery bookkeeping/observability rather than tenant data exposure.
     Evidence: services/recovery-orchestrator/cmd/server/main.go lines 443-474.
     Confidence: Plausible (concurrency-flavored; a full exploit chain would need a second specialist pass
     for a race-timing PoC, noted under Suspicions).

Suspicions requiring validation:
  - Finding #1 (RLS bypass) should be validated end-to-end against a live cluster provisioned exactly the
    way deploy/native/install.sh + postgres-setup.sh sets one up: connect as the resulting DATABASE_URL
    role, set `scos.tenant_id` to a foreign tenant, and confirm cross-tenant rows are returned. I did not
    have a live Postgres instance in this pass to run that confirmation query; the finding rests on reading
    the provisioning scripts plus documented PostgreSQL RLS-owner-bypass semantics, both of which are
    unambiguous, but a live confirmation would move this from Confirmed-by-inspection to fully proven.
  - Whether any operator-documented production runbook (outside this repository, e.g. Terraform/Ansible not
    present here) creates a distinct least-privilege role and grants it before use — I found no such script
    inside this repository, but cannot rule out an out-of-repo provisioning step. Worth an explicit question
    to the team.
  - Finding #5's practical race window (two /v1/recovery/start calls close enough together) was not
    exercised under mcp__scos tools or a live server in this pass — reasoning is from source inspection only.
  - Finding #3's temp-dir permissions: os.CreateTemp defaults to 0600, which is correct; the residual risk
    is specifically the crash-before-cleanup window and any other local principal/process with root or the
    same-user access to the temp directory, not a permissions misconfiguration per se.

Tests/commands executed:
  - `find services/backup-manager services/recovery-orchestrator services/security-gate database/migrations
     -type f | sort`
  - `wc -l database/migrations/*.sql`
  - `grep -l -i "down\|rollback" database/migrations/*.sql` (no output — confirms no rollback files)
  - `grep -n "^BEGIN;\|^COMMIT;\|^BEGIN$\|^COMMIT$" database/migrations/*.sql` (confirms per-file
     transaction wrapping in all 20 files)
  - `grep -n "CREATE TABLE\|CREATE INDEX\|ALTER TABLE\|CREATE UNIQUE INDEX\|CREATE OR REPLACE"
     database/migrations/*.sql | grep -v "IF NOT EXISTS\|OR REPLACE"` (confirms non-idempotent DDL)
  - `cat scripts/scos-migrate` (full read)
  - `grep -rl "migrations"` across the repo to locate the runner and its callers
  - `grep -n -A10 -B5 "migrations" deploy/native/postgres-setup.sh deploy/native/install.sh`
  - Full reads of services/backup-manager/cmd/server/main.go, services/recovery-orchestrator/cmd/server/
    main.go, services/security-gate/cmd/server/main.go, services/security-gate/cmd/server/store.go,
    packages/auth/middleware.go
  - `grep -rn "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9\|mintClaims\|IssuerSecret\|\"exp\"" --include="*.go"`
     to find all claims-token minters
  - `grep -rn "FORCE ROW LEVEL SECURITY\|ENABLE ROW LEVEL SECURITY" database/policies/*.sql
     database/migrations/*.sql | wc -l` and `grep -rln "FORCE ROW LEVEL SECURITY" ...` (0 matches)
  - `ls database/policies/`, spot-read of database/policies/*.sql and README.md
  - `grep -n "POSTGRES_USER\|CREATE ROLE\|CREATE USER\|GRANT\|OWNER TO" deploy/compose/docker-compose.yml
     deploy/native/postgres-setup.sh`
  - `grep -n "CREATE ROLE\|GRANT\|SET ROLE\|DATABASE_URL\|POSTGRES_USER\|FORCE ROW\|BYPASSRLS\|connect"
     database/integration/rls_test.go`
  - `grep -n "POSTGRES_USER\|DATABASE_URL\|SCOS_DATABASE_URL" deploy/compose/docker-compose.yml`
  - `grep -rn "FORCE ROW LEVEL SECURITY" database/` (final confirmation, no matches, exit 1)

Coverage gaps:
  - No live PostgreSQL cluster was stood up in this pass to empirically confirm Finding #1 by actually
    connecting as the provisioned role and observing cross-tenant rows; the finding is based on reading the
    provisioning scripts and documented Postgres semantics, not a runtime reproduction.
  - services/backup-manager/cmd/server/main_test.go, services/recovery-orchestrator/cmd/server/main_test.go,
    services/security-gate/cmd/server/main_test.go, and database/integration/rls_test.go were located but
    not fully read line-by-line in this pass; they were used only to confirm the non-owner test-role pattern
    for Finding #1. A full test-gap comparison (what these tests do vs. do not cover for the findings above)
    was not performed — that is properly the hostile-test-gap-auditor's scope, but is flagged as a natural
    follow-up.
  - services/backup-manager's non-production ("local") path (verifyBackup, cleanupRetention, handleRestore
    for provider == "local") was read but not adversarially tested against retention-cleanup partial
    failures beyond the single os.Remove error return already visible in cleanupRetention.
  - services/security-gate's securestoreClient retry/backoff behavior (there is none — "getSecret ... never
    retries") was read but I did not trace Securestore's own failure semantics (rate limiting, partial
    responses) since that platform's code is out of this repository.
  - database/migrations files 000002-000020 were grepped/sampled for the specific patterns above (BEGIN/
    COMMIT, IF NOT EXISTS) but not read end-to-end for unrelated correctness issues outside this audit's
    error-path/rollback scope.
  - Did not trace how database/migrations interacts with database/policies at deploy time beyond
    scripts/scos-migrate's `for file in /scos-database/migrations/*.sql /scos-database/policies/*.sql`
    loop (confirmed both directories are applied through the same tracked, non-idempotent mechanism).
  - Did not review services/execution-proxy or services/tool-governance in this pass (out of the assigned
    scope hint), despite the "known context" note flagging them as security-relevant; another specialist's
    scope per the task framing.
  - Did not review services/mcp-gateway's error paths despite it being the externally-reachable surface
    named in "known context" — out of the explicit scope hint for this pass.

Areas explicitly not reviewed:
  - services/execution-proxy, services/tool-governance, services/mcp-gateway (assigned to other specialists
    per the task's own framing / out of this pass's scope hint).
  - Non-Go infrastructure code beyond deploy/native and deploy/compose/docker-compose.yml (e.g. any
    Kubernetes manifests, Terraform, or CI/CD pipelines) that might independently provision the database
    role differently from what deploy/native and deploy/compose show.
  - The full contents of database/migrations/000002 through 000020 (only grepped for the specific patterns
    listed under Tests/commands executed, not read end-to-end).
  - packages/database (RequireTLS, TuneSQLDB, NewPool) beyond the calls already visible in the three
    services read; not independently audited for its own error paths in this pass.

Recommended follow-up:
  1. (Critical, urgent) Add `ALTER TABLE ... FORCE ROW LEVEL SECURITY` to every RLS-protected table across
     database/policies (or add a single migration that forces it on all of them), AND update
     deploy/native/postgres-setup.sh + deploy/compose/docker-compose.yml (and any out-of-repo provisioning)
     to create a distinct, least-privilege, non-owner application role — mirroring
     database/integration/rls_test.go's `NOSUPERUSER NOBYPASSRLS` role — and wire every service's
     DATABASE_URL/SCOS_DATABASE_URL to that role instead of the migration/owner role. Then re-run
     database/integration/rls_test.go's scenario against that exact deployment configuration (not just the
     test's own fixture role) to close Finding #1's remaining live-verification gap.
  2. Make scripts/scos-migrate atomic per file: wrap the DDL execution and the
     `INSERT INTO scos_schema_migrations` bookkeeping in the *same* transaction (e.g., by appending the
     INSERT to the end of each file's own BEGIN/COMMIT block, or by having scos-migrate pipe both the file
     contents and the bookkeeping INSERT into one `psql -f` invocation), and/or make the DDL idempotent
     (IF NOT EXISTS) so a re-run after interruption is safe either way. Separately, decide and document a
     real rollback story for database/migrations (either paired down-files or a documented
     forward-fix-only policy) so "rollback" is not silently undefined.
  3. In services/backup-manager, either stream pg_dump output directly into the encryption step without
     touching disk in plaintext, or write the plaintext dump under a directory with the same access
     controls as backupDir and add a startup-time sweep that finds and deletes any orphaned
     `scos-pg-dump-*` files left by a prior crashed run.
  4. In packages/auth/middleware.go, require "exp" to be present and non-zero for a claims token to
     validate at all (fail closed on a missing exp rather than treating it as non-expiring).
  5. In services/recovery-orchestrator, make the recovery_leases upsert conditional — only acquire when no
     row exists or the existing row's state is not 'active'/is expired — and surface a 409-style error to
     the caller of /v1/recovery/start when a lease is already held by a different, live recovery_run_id.
```
