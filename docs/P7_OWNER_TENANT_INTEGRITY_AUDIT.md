# P7 memory owner/tenant integrity preflight

## Scope

Migration `000091_memory_item_owner_tenant_fk.sql` installs the composite
`memory_items(tenant_id, owner_principal_id)` foreign key against
`principals(tenant_id, id)` as **NOT VALID**. New writes are checked,
but existing malformed rows are not repaired, deleted, or validated.

`tools/owner-integrity-audit` provides a **read-only, aggregate-only**
inventory for that remaining rollout gate. It is not a repair tool, migration
runner, constraint validator, recovery drill, or approval to operate on a
production database.

## Execution

Use an approved, privileged migration-owner connection with TLS enforced.
The tool reads `DATABASE_URL` from the environment, never a command-line
argument. For example, after supplying the DSN through an approved secret
mechanism:

```sh
go build -o /tmp/scos-owner-integrity-audit ./tools/owner-integrity-audit
/tmp/scos-owner-integrity-audit
```

Use the built executable when checking exit codes; `go run` can wrap a
program's nonzero exit status in its own exit code.

The command requires `sslmode=require`, `verify-ca`, or `verify-full`.
It uses a repeatable-read, **READ ONLY** PostgreSQL transaction, a 30-second
statement timeout, and `row_security=off`. The latter causes an error for
a role whose RLS policies would hide rows, instead of returning an incomplete
inventory as if it were complete. Use a role authorized to inventory every
tenant; do not weaken policies or grant new privileges to run this audit.

Output is one JSON object with the total memory-item count, counts of
missing owner principals and cross-tenant owner principals, and whether the
correctly shaped composite foreign key exists and is validated. No tenant,
principal, or memory identifiers, content, or connection strings are printed.

Exit codes:

- `0`: query completed, expected FK exists, and both violation counts are
  zero. If `constraint_validated=false`, validation is **still pending**.
- `2`: missing FK or one or more owner integrity violations.
- `1`: connection/TLS, authorization/RLS, query, catalog-definition,
  timeout, or JSON-output failure. Treat this as **unknown**, not clean.

A same-named FK with incorrect source columns, referenced columns, or
referenced table is rejected. The report is only a point-in-time inventory;
a clean report is **not** proof of successful validation or production
acceptance. Run the preflight again before an independently approved
`VALIDATE CONSTRAINT` and follow the separate rollback/recovery runbook.
Never automatically reassign a source owner: ownership is part of P7 review
provenance and requires a documented authorization and audit trail.

## Test evidence, 2026-10-30

On AMD, a disposable PostgreSQL 17/pgvector instance installed all 90
migration files and 23 RLS policy files. Before migration 000091, a synthetic
memory item was deliberately assigned to a principal in another tenant.
After installing the NOT VALID FK, the audit reported one cross-tenant
violation and exited 2. Test-only remediation changed that synthetic row
to a same-tenant owner; the audit then reported zero violations and exited 0,
while correctly retaining `constraint_validated=false`.

The P4 PostgreSQL CI workflow also runs the owner-integrity integration
regressions against its disposable database. That workflow's test DSN is
local-only; the production audit executable independently requires TLS.

Transaction-rolled-back PostgreSQL integration fixtures exercise missing
owner, cross-tenant owner, repaired-but-unvalidated FK, validated FK,
misleading same-named FK, and FORCE RLS filtering. Five repeated focused
integration runs passed. A deliberate query mutation suppressing
cross-tenant detection made the regression fail, proving it catches that
class of false-clean result. The complete uncached Go suite passed in
53 tested packages, `go vet ./...` passed, and diff checks passed.

These are **local source and disposable-database tests**. No production
inventory, remediation, constraint validation, migration, deployment, or
recovery test was performed. P7 and governance §41 remain incomplete.
