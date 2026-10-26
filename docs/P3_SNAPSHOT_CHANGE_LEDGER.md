# P3 — Durable snapshot change evidence (migration 000072)

SCOS records file-change evidence in the **same PostgreSQL transaction**
as snapshot activation. Failed indexing cannot leave committed events.

Records include the tenant, repository, snapshot, change kind
(add/modify/delete/rename/copy), path, previous path and evidence class:

- exact_path: same path with changed content or language
- snapshot_manifest: appeared or disappeared; origin not provable
- unambiguous_content: unique prior path with matching language and content
  proves conservative copy or rename continuity

Ambiguous identical-content origins are not called renames or copies.
The events are server-derived from indexed manifests, never trusted
client-provided Git sidecar claims.

Forced row-level security scopes tenant reads and inserts. The restricted
runtime role cannot update or delete existing event records. The
fresh-install policy and migration both enforce these privileges.

For an authorized SQL transaction, use a verified tenant identity to set
scos.tenant_id transaction-locally before selecting this ledger.

This is a native IndexSnapshot persistence slice. It does not yet
verify Git sidecar claims, expose an authenticated agent-facing event
endpoint, persist SCIP-only imports, handle very large index payloads,
or establish large-repository performance superiority.

Verified against disposable PostgreSQL 17 and the complete native Go
integration suite with initial/changed snapshots, copy/rename ambiguity,
rollback, and restricted application-role tenant isolation.
