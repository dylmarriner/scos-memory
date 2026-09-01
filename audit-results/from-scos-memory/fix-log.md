# Fix log

AUDIT-005: deploy/compose/docker-compose.yml now binds published ports to
127.0.0.1 and fails closed when `POSTGRES_PASSWORD` is unset. Root cause: short
host:container syntax exposed internal services and the old password default was
`change-me`. Verification: compose config passes when required secrets are
provided; loopback binding is not a substitute for service authentication.

AUDIT-001..004: implemented shared HMAC bearer authentication across all HTTP
services, issuer-authenticated token issuance, owner-bound execution results/raw
restores, server-owned operation IDs, and HMAC manifest verification. Regression
tests cover token tampering, expiry, issuer omission, and package behavior.

AUDIT-006: tool-governance authenticated every mutating route with a scope
(`tools:admin`, `operations:execute`, `operations:complete`,
`operations:reconcile`) but never bound tools/operations to the tenant that
created them, and the list/get endpoints returned every tenant's records to
any authenticated caller. A caller from tenant-b holding `tools:admin` could
transition tenant-a's tools, start operations against tenant-a's authorised
tools, and complete/reconcile tenant-a's operations — this is the "durable
principal-bound transitions" follow-up flagged in AUDIT-003/the final report.
Root cause: `ToolDefinition`/`ToolOperation` had no `Tenant` field and no
handler compared the caller's verified identity to resource ownership.
Fix: `services/tool-governance/cmd/server/main.go` now stamps
`Tenant`/`Owner` from the verified bearer identity at registration/start
(never from caller-supplied fields) and every read/mutate handler calls a new
`tenantAuthorized` helper that requires `identity.Tenant == resource.Tenant`
unless the caller holds the pre-existing "admin" scope (an intentional
cross-tenant escape hatch, consistent with `serviceauth.HasScope`). List
endpoints filter to the caller's tenant. Verification: `go build ./...`,
`go vet ./...`, and `go test ./...` all pass; new tests
`TestToolGovernanceTenantIsolation` and
`TestToolGovernanceAdminScopeCrossesTenants` drive the real HTTP handlers
through `serviceauth.Require` with two distinct signed bearer tokens and
assert tenant-b gets 404 on every tenant-a resource while tenant-a and an
admin-scoped caller succeed. Residual risk: state is still in-memory (lost on
restart, no durable audit trail of who performed a transition) — that part of
the original follow-up (a database-backed control plane) is unaddressed.

AUDIT-007: `recovery-orchestrator` and `backup-manager` were the only two
services in the repository whose mutating routes had no `serviceauth.RequireScope`
gate at all — only the blanket bearer-authentication check from
`serviceauth.Require`. Any authenticated caller, including one with a token
scoped for something unrelated, could: force `/v1/recovery/complete` to mark
a service's disaster-recovery checklist "completed" without any checklist
step ever running, silently masking a degraded service; falsify
`/v1/backups/complete` and `/v1/restore-tests/complete` verification status,
size, and location without a backup or restore ever actually happening; and
read every service's recovery/backup state (`/v1/recovery/status`,
`/v1/backups`, `/v1/restore-tests`). This is the "authorization gap" flagged
against the Recovery/backup row of the workflow matrix in the final report.
Root cause: these two services were written without the
`serviceauth.RequireScope` gate already used by tool-governance and
extension-manager for equivalent operator-only actions. Fix:
`/v1/recovery/start`, `/v1/recovery/checklist`, and `/v1/recovery/complete`
now require `recovery:admin`; `/v1/backups/start`, `/v1/backups/complete`,
`/v1/restore-tests/start`, and `/v1/restore-tests/complete` now require
`backups:admin`. Read-only status/list endpoints were left open to any
authenticated caller, matching the existing convention elsewhere (these
services have no tenant concept to scope reads by). Verification:
`go build ./...`, `go vet ./...`, `go test ./...` all pass; new tests
`TestRecoveryMutationsRequireAdminScope` and
`TestBackupMutationsRequireAdminScope` drive the real HTTP handlers through
`serviceauth.Require` and assert a scopeless bearer token gets 403 on every
mutating route while an `*:admin`-scoped token succeeds. Residual risk: state
remains in-memory (no durable audit trail across restarts); scope issuance
itself still depends on the issuer (security-gate) being configured
correctly — this fix only closes the missing-authorization gap inside these
two services, it doesn't change how `recovery:admin`/`backups:admin` tokens
are granted operationally.
