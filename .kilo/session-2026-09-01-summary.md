# Session summary — 2026-09-01

Covers all work from `016996e` (17:31) through `7cf525c` (23:38), plus one
uncommitted fix still sitting in the working tree. Written for handoff to
Kilo — read this before touching `code-intelligence`, `graphify-provider`,
or `deploy/scripts/auto-deploy.sh`.

## Uncommitted, not yet committed

**`services/graphify-provider/cmd/server/graphify.go` /
`graphify_test.go`** — production graphify (0.9.52) silently changed its
node-link JSON edge key from `"links"` to `"edges"` (networkx's
`node_link_data` default changed, graphify inherited it with no version
marker). The service was decoding only `"links"`, so every edge from a
live 0.9.52 extraction was dropped with no error — node count looked
right, graph was just edgeless. Fixed with a custom `UnmarshalJSON` that
tries `"links"` then falls back to `"edges"`. Two new fixtures added:
`testdata/graph_0.9.52.json` (real 0.9.52 output, has edges under
`"edges"`) and `testdata/graph_no_edges.json` (regression guard for the
genuinely-edgeless case). **Also untracked: `.kilo/` itself** — contains
`audit-remediation-plan.md`, a still-open plan for CSRF tokens + a
transactional team-invitation-capacity fix in `cloud-control-plane`
(not started).

Full validation (`go build ./... && go vet ./... && go test ./...`) was
run in the background after this fix and completed with exit code 0.

## What shipped this session (commit order)

1. **`016996e`** — synced the local repo with the production Linode
   server's source, which had drifted out of git entirely. Pulled in
   `.claude/.codex/.agents` skill dirs, a hostile-repo-audit workflow, etc.
2. **`78755c4`** — added auto-deploy: a systemd timer polls
   `origin/main` and rebuilds/redeploys on new commits
   (`deploy/scripts/auto-deploy.sh` + systemd unit/timer).
3. **`67e2e59`, `a234601`** — added `SCOS_MEMORY_COMPETITIVE_AUDIT_AND_MASTER_PLAN.md`
   and `SCOS_MEMORY_TECHNICAL_BLUEPRINT...` docs (roadmap/positioning docs).
4. **`e643b67`** — fixed graphify's cache to detect stale graphs when repo
   content changes (previously could serve a cached graph past a repo
   change).
5. **`c91874d`** — added a release parity manifest so deploys can verify
   what actually shipped (image digests vs. what's running).
6. **`9854a3c`** — **security fix**: cross-tenant IDOR in
   extension-manager. Added a tenant-isolation regression suite across
   several services.
7. **`9bade7c`** — added Phase 0 ground-truth fixtures for
   code-intelligence ahead of a rewrite.
8. **`35c5d3c`** — added the durable, snapshot-based code store (Phase
   2/3): new `000025_code_intelligence_persistence.sql` migration,
   `pgstore.go`/`pghandlers.go`, fixing prior rename- and branch-tracking
   gaps.
9. **`578cadb`** — fixed auto-deploy so it actually re-runs new DB
   migrations on redeploy (it wasn't).
10. **`a0b8e91`** — added lexical (Postgres FTS) code search with
    automatic fallback.
11. **`48f7a6e`** — added snapshot diffing: parent linkage, change
    classification, and a diff endpoint.
12. **`0a8a88b`** — readiness check now verifies durable code store
    connectivity when configured.
13. **`5fd1ae6`** — made the durable code store **always-on** by default,
    with a per-user opt-out (dashboard toggle + migration
    `000027_code_intelligence_user_settings.sql`).
14. **`1356ccd`** — added real Go declaration extraction via `go/parser`
    (Tier 1 semantic symbols) — `goextract.go`.
15. **`b9a9c1c`** — added real syntactic call-edge extraction for Go,
    populating `code_edges`.
16. **`059ba02`** — added real change-impact analysis using the new call
    graph.
17. **`64c63b7`** — added SCIP ingestion (P0-006): import real semantic
    indexes from any language, not just Go.
18. **`7cf525c`** — fixed the deploy parity check: it compared a bare
    image digest against a `sha256:`-prefixed one and reported false
    mismatches for all 21 services on 2026-09-01 even though the deploy
    had actually succeeded. Found live on the production server.

## Themes

- **code-intelligence went from stub to real**: ground-truth fixtures →
  durable Postgres-backed store → lexical search → snapshot diffing →
  real Go AST extraction (declarations, then call edges) → change-impact
  analysis on the real call graph → SCIP ingestion for other languages.
  This is the P0 roadmap items being worked through in order.
- **Deploy pipeline hardening**: auto-deploy added, then two bugs found
  live on production and fixed same-day (migrations not re-running,
  parity check false-negative on digest format).
- **One real security fix**: cross-tenant IDOR in extension-manager.

## Open / next

- `.kilo/audit-remediation-plan.md`'s two tasks are **not started**: CSRF
  tokens for cookie sessions, and making team-invitation capacity
  enforcement transactional in `cloud-control-plane`.
- The graphify edges-key fix above needs to be committed.
- Production Linode server: `ubuntu-sg-sin-2`, IP `172.236.155.15`
  (per earlier session notes — verify still current before relying on it).
