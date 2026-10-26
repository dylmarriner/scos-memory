# P7 revalidation-decision purge: privileged operator procedure

**Status:** Review-branch implementation only. This is not authorization to purge
tenant data, and it does not constitute a complete tenant-offboarding workflow.

## Why this exists

`memory_revalidation_decisions` contains immutable human assessments. Its
foreign keys intentionally prevent deleting referenced revalidation events or
tenants while decisions remain. Migration
`000071_revalidation_privileged_purge.sql` adds an explicit, narrowly scoped
escape hatch for **approved tenant offboarding only**, not routine cleanup.

## Authority and safety properties

- Only the database migration-owning role may invoke
  `purge_memory_revalidation_decisions_for_tenant(uuid,text)`. Public and
  `scos_app` EXECUTE are revoked; `scos_app` cannot DELETE decisions or read
  the privileged purge ledger.
- The procedure locks the tenant row, counts its decisions, inserts a durable
  audit record, deletes only that tenant's decisions, and checks the count.
  Any error rolls back both the deletion and audit.
- The delete trigger permits only a migration-owner DELETE in the same
  transaction as a matching audit record. Ordinary UPDATE remains forbidden.
  A caller-settable session GUC is **not** an authorization bypass.
- The purge ledger records tenant UUID, database session identity, reason,
  transaction ID, count, and timestamp. It is append-only and deliberately
  has **no foreign key to tenants**, so evidence can outlive tenant deletion.
- No endpoint or MCP tool exposes this privileged operation.

## Operator workflow (requires explicit owner approval)

1. Verify approval, tenant identity, legal retention requirements, backups,
   and the **entire** tenant-deletion dependency graph. Other tables can still
   block tenant deletion; this procedure resolves only the P7 decision
   dependency. Do not treat this as a complete offboarding implementation.
2. Connect as the migration owner, not `scos_app`. Open a single transaction.
3. Invoke the procedure with the exact approved tenant UUID and a
   meaningful 16–2000-character rationale:

   ```sql
   BEGIN;
   SELECT purge_memory_revalidation_decisions_for_tenant(
       '<approved-tenant-uuid>'::uuid,
       'Approved tenant offboarding, change ticket <ticket-id>'
   );
   -- Verify the returned count and perform the separately approved,
   -- dependency-aware tenant offboarding in this SAME transaction.
   -- If any dependency fails, ROLLBACK instead of committing a partial purge.
   COMMIT;
   ```

4. Verify the offboarding result and immutable ledger under the privileged
   account. Store approval/change-ticket evidence separately. The audit ledger
   is not a substitute for a backup or legal retention policy.

## Regression evidence

`TestPGRevalidationGovernedDecision` uses real PostgreSQL and the restricted
`scos_app` role to prove direct owner deletion fails without an audit,
runtime purge/DELETE/ledger access are denied, rollback restores decisions
without an audit, an approved-role purge removes only targeted decisions, and
the resulting ledger cannot be updated or deleted. These tests do **not**
prove that all unrelated tenant foreign keys are ready for full offboarding.

**Never run this against production without separate explicit approval.**
