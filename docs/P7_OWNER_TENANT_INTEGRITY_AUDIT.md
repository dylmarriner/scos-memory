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
# Only after separately approved migration, remediation and FK validation:
/tmp/scos-owner-integrity-audit --require-validated
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

- `0` (default preflight): query completed, expected FK exists, and both
  violation counts are zero. If `constraint_validated=false`, validation is
  **still pending**.
- `0` (`--require-validated`): in addition to a clean inventory, the expected
  FK is catalog-confirmed as validated. With this flag, an otherwise clean
  `NOT VALID` FK returns `2`, never a false completion signal.
- `2`: missing FK, one or more owner integrity violations, or an unvalidated
  FK when `--require-validated` is supplied.
- `1`: connection/TLS, authorization/RLS, query, catalog-definition,
  timeout, or JSON-output failure. Treat this as **unknown**, not clean.

A same-named FK with incorrect source columns, referenced columns,
referenced table, referential DELETE/UPDATE actions, or match type is rejected. The report is only a point-in-time inventory;
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

## PR #169 review hardening (2026-10-30)

A real PostgreSQL integration test reproduced a false-clean report when a
same-named composite FK used `ON DELETE CASCADE` (RED). The catalog check now
compares `confdeltype`, `confupdtype`, and `confmatchtype` with migration 000091's
NO ACTION / NO ACTION / MATCH SIMPLE semantics (GREEN). A separate connection
regression verifies the actual audit transaction is repeatable-read and
read-only, including rejecting an attempted temporary-table write.

A subprocess integration regression runs the built CLI over a dedicated,
TLS-enabled disposable PostgreSQL database with committed public-schema
fixtures: a cross-tenant legacy owner returns exit 2, and test-only deletion
of that synthetic bad row returns exit 0. The subprocess test requires both
`SCOS_OWNER_AUDIT_TLS_TEST_DSN` and `SCOS_OWNER_AUDIT_DISPOSABLE=1`, and refuses
non-TLS connections, unexpected database names, or pre-existing target tables.
It skips without a dedicated TLS fixture; the standard CI integration suite
continues to cover the catalog and transaction checks. No live database was
queried, changed, repaired, or validated.

## Separate validation-state gate (2026-10-30)

The default command intentionally remains a preflight so operators can
inventory legacy rows before an approved validation. The optional
`--require-validated` switch is a stricter **read-only** acceptance check;
it never runs `ALTER TABLE`, repairs data or grants authorization to deploy.

TDD first failed to compile because the acceptance decision did not exist.
Six exit-code unit cases then passed, covering missing FK, missing and
cross-tenant owners, clean unvalidated preflight, rejected unvalidated
acceptance and accepted validated state. Real PostgreSQL 17 integration
asserted the same transition inside a rolled-back fixture. A built-binary
subprocess test using a separate disposable TLS-enabled PostgreSQL database
confirmed `--require-validated` exits 2 for a clean NOT VALID constraint and
exits 0 after a **test-only** `VALIDATE CONSTRAINT`. This does not establish
that any deployed database is clean or validated; rollout and recovery
qualification remain explicitly outstanding.
