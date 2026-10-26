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
verify Git sidecar claims, persist SCIP-only imports, handle very large
index payloads, or establish large-repository performance superiority.

Verified against disposable PostgreSQL 17 and the complete native Go
integration suite with initial/changed snapshots, copy/rename ambiguity,
rollback, and restricted application-role tenant isolation.


## Authenticated agent-facing event retrieval (development PR)

`POST /v2/code/snapshots/events` accepts a JSON object with a required
`snapshot_id` UUID, optional `limit` (1–1000, default 100), and optional
`after_path` keyset cursor. The response contains `snapshot_id`, `repository_id`, `branch`,
`commit_id`, `parent_commit_id` (empty for an initial snapshot), ordered
`events` with immutable ID, change kind, path, previous path (when
applicable), evidence class and timestamp, and `next_after_path` when another
page exists. Paths are ordered using PostgreSQL's C collation, including the
cursor comparison, to avoid locale-dependent skipped or duplicated pages.

This route follows the same authenticated service boundary and caller-specific
durable-store preference as the other v2 code routes. Tenant ownership is
checked against both snapshot and repository before reading; a foreign,
missing, or incomplete snapshot returns the same HTTP 404. A malformed UUID,
unknown field or invalid page size returns 400. GET returns 405.

The MCP gateway exposes `scos_code_snapshot_events` with the same selectors.
It forwards identity established by the gateway, not caller-supplied tenant
headers. The endpoint only reads server-derived evidence. It does **not**
accept client-provided Git events, authorize code changes, or reconcile Git
provenance. Read-only historical snapshots remain queryable after supersession.

Real PostgreSQL integration tests cover pagination, tenant isolation, auth,
incomplete snapshots, historical retrieval and add/modify/delete evidence;
gateway tests verify upstream route, validated identity propagation and
argument rejection. These tests do not prove production deployment.

The authenticated event endpoint uses migration 000073 to index
`(snapshot_id, path COLLATE "C")`, matching the deterministic keyset cursor
on databases whose default collation is not C. The index is created
CONCURRENTLY by the non-transactional migration runner to avoid blocking
production writes during a future separately authorized deployment. The restricted-role HTTP
regression establishes the verified tenant context within the read transaction.

## Server-recorded commit binding (P3, 2026-10-26)

The authenticated event page now includes immutable repository, branch,
target commit and parent commit identifiers from the snapshot rows, not
caller-supplied event metadata. Every page repeats those fields. The offline
Git verifier rejects a wrong/missing target or parent commit, cross-page
repository/branch changes, and incomplete exports. Real-Git Python fixtures
and PostgreSQL 17 HTTP integration tests cover the failure cases, including
tenant-scoped restricted-role reads. On the isolated development worktree,
the uncached full Go suite passed against a disposable PostgreSQL 17 with
all 74 migrations; go vet and 34 Python Git-worklist/verifier fixtures
passed. These are source-level checks, not deployed-system evidence.

**Trust boundary:** commit IDs were supplied by the index request. This
binds the export to the server's *recorded* snapshot identity but does not
attest that uploaded source bytes match the Git commit tree. A forged Git
object or incorrect source payload can still carry the claimed commit ID.
End-to-end Git tree/source attestation, authenticated export provenance,
and large-repository acceptance remain open. No production deployment.
