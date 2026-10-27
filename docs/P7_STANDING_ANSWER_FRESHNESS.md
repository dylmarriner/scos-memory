# P7 standing-answer evidence freshness (migration 000084)

This is a **database-level invalidation slice**, not a complete cache service,
an automatic answer generator, or production deployment.

## Provenance and write contract

The migration adds \`stale_at\` and \`stale_reason\` to
\`memory_standing_answers\`, plus typed
\`memory_standing_answer_dependencies\` with memory UUIDs and the supporting
version observed at refresh. The dependencies are trigger-maintained and not
directly readable or mutable by the restricted runtime role.

A standing answer may be inserted/refreshed as fresh only when its JSON
\`evidence_fact_ids\` is a nonempty UUID-string array of real active facts
in the same tenant and namespace. Each fact must be effective and unexpired,
below the namespace sensitivity ceiling, not superseded, and free of unresolved
P7 code-revalidation events. A reviewed \`still_valid\` decision permits the
evidence to be used again; superseded, invalidated and unresolved decisions do
not silently restore freshness.

Existing standing answers are conservatively marked
\`unverified_legacy_evidence\` during migration. The migration does **not**
pretend their historical free-text evidence lists are authoritative.

## Automatic stale transitions

In the same PostgreSQL transaction as a fact status, version, supersession,
effective-date, expiry, sensitivity or namespace update, only standing
answers linked to that fact are marked stale. Deleting a fact marks its
dependent answers stale before evidence FK cascade. Unrelated metadata writes
do not mark them stale.

A newly inserted P7 revalidation event requiring review also marks the
answers dependent on its fact stale, without editing memory truth or making
any human review decision. An attempted refresh using unresolved P7 evidence
fails. The cache remains stale until an authorized, evidence-valid refresh.

This does **not** continuously detect when the wall clock passes a previously
future \`valid_until\` or \`expires_at\`. Any future standing-answer read path
MUST check \`stale_at IS NULL\` **and live evidence validity at read time**.
The existing source tree does not contain an active standing-answer read
service. Merely writing these schema rules is not proof of serving fresh
answers and does not close the P7 acceptance gate.

## Isolation and recovery

- Migration \`000084\` is append-only and does not modify any production data
  without an approved deployment.
- The dependency table uses forced RLS and an owner-only maintenance policy;
  \`scos_app\` direct table privileges are revoked both during upgrades and
  after the fresh-install blanket grant in \`database/policies/000011_app_role.sql\`.
- Privileged trigger operations occur in the same transaction as the memory
  state or P7 event. Failed cache evidence validation rolls back the write.
- The existing standing-answer namespace-membership policy is retained.
- Automatic invalidation does not delete audit evidence or rewrite approved
  memory facts.

## Verification

The new real PostgreSQL integration tests under the restricted app role
cover exact dependency registration; unrelated metadata changes; status
supersession; fact deletion; attempted stale-answer reactivation; approved
reviewer \`still_valid\` recovery; changed-code \`needs_review\` invalidation;
cross-tenant evidence rejection; and direct dependency-table denial.

Run with a disposable migrated PostgreSQL 17/pgvector database with all
policies and both \`DATABASE_URL\` (migration owner) and
\`SCOS_APP_TEST_DATABASE_URL\` (non-superuser, non-BYPASSRLS runtime role):

\`\`\`bash
go test -tags=integration -count=1 ./services/memory-intelligence/cmd/server \
  -run '^TestP7StandingAnswer'
\`\`\`

This release requires GitHub CI, independent review, source merge, merged-main
tests and a separately approved live rollout before being considered deployed.
