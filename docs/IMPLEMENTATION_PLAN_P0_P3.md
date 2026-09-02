# scos-memory: Full P0–P3 Implementation Plan

## Implementation status (updated 2026-09-02)

- **P0-0 (Make RLS actually enforce): implemented, not yet live-verified.** All files below are written into this repo and `go build ./...`/`go vet ./...` are clean, but `database/integration/rls_deployed_role_test.go` still needs a run against a live Compose stack with `SCOS_APP_DATABASE_URL` set (it's gated on a real deployment, not just any Postgres) before this ships. Treat "written and compiling" and "live-verified" as separate claims here.
  - New: `database/policies/000010_capabilities_rls.sql`, `000011_app_role.sql`, `000012_force_rls.sql`, `docs/adr/0012-rls-enforcement.md`, `packages/database/rls_guard.go`, `database/integration/rls_deployed_role_test.go`
  - Modified: `scripts/scos-migrate`, `deploy/compose/docker-compose.yml`, `deploy/native/postgres-setup.sh`, `.env.example`, `packages/database/pool.go`
- **P0-A (Graph-centrality code context ranking): implemented and verified**, against a disposable Postgres container with all 29 migrations applied — differs from the design below in one respect: implemented as a small pure-Go personalized-PageRank (`services/code-intelligence/cmd/server/pagerank.go`), not `gonum.org/v1/gonum/graph`, to avoid a new dependency for what's a ~100-line algorithm over an adjacency map already shaped that way; the ranked-field/ChangeImpact wiring matches the design as written.
  - New: `services/code-intelligence/cmd/server/pagerank.go`, `pagerank_test.go` (4 unit tests on the ranking algorithm itself: hub-outranks-leaf, seed biases toward neighbors, dangling mass doesn't leak, empty input)
  - Modified: `services/code-intelligence/cmd/server/pgstore.go` (`PGSymbolVersion.Centrality`, `ChangeImpact` now ranks `DirectDependents`/`TransitiveDependents` by personalized PageRank seeded on the matched symbols, new `edgesAmong` helper scoped to the BFS-visited subgraph — not the whole repository graph), `pgstore_integration_test.go` (`TestPGChangeImpactRanksDependentsByCentrality`, a fan-in fixture asserting a caller-of-a-caller outranks a dead-end caller)
  - Not done: `QueryByPathOrFQN` has no ranking field — left alone deliberately, since it resolves to an exact path/FQN match (0 or 1 result in practice), where a centrality score adds no decision value; a dedicated `scos_code_rank` MCP tool (mentioned as a possible follow-on in the design below) was not built.
- **P0-B, P0-C, and everything P1–P3**: designed below, not yet built.

---

## Context

The 2026-09-01 competitive audit (`SCOS_MEMORY_COMPETITIVE_AUDIT_AND_MASTER_PLAN.md`) produced a prioritized backlog (§24, 16 items across P0–P3) after benchmarking scos-memory against seven real competitors. You asked for an implementation plan covering that **entire backlog**, not just the top item.

Before writing this, I did the research the audit itself flagged as missing — four items were explicitly marked "verify before building," and I resolved all four against current source rather than carrying the audit's assumptions forward:

- **P0-0 (RLS enforcement)** — fully scoped: exact role split, every affected table, and a **real policy gap I found that the audit didn't** (`capabilities` table has RLS enabled with zero policies — would silently break capability registration the moment enforcement goes live).
- **P0-B (PII/secret scanning)** — the audit left this "Unknown, check first." I checked. **It's absent.** `security-gate`'s only scanning-adjacent code is a keyword classifier (`classifyContent`) not wired into any write path. This is a real build, not a five-minute verification.
- **P0-C (contradiction check)** — the audit assumed `fact_key_norm`/`fact_value_norm` normalization already exists and just needs wiring in. **It doesn't exist at all** — `Fact` has no key/value decomposition, only free-text `Content`. This changes P0-C from a trivial heuristic tweak into a real (if still small) feature.
- **P1-D (incremental indexing)** — confirmed: `IndexSnapshot` writes every file unconditionally, no content-hash skip exists yet.
- **P0-A (PageRank ranking)** — confirmed no graph library (`gonum`, `networkx`) is a dependency anywhere in the repo today.

Where the audit's complexity/risk estimate no longer holds given what I found, I've corrected it below and said so — I'm not going to let a wrong assumption ride just because the audit already wrote a number down.

**Scale note, stated plainly:** this is 16 backlog items spanning a database security fix, two new ML-adjacent subsystems (cross-encoder rerank, PageRank ranking), three schema additions, one new safety-scanning subsystem, and three explicitly research-grade items (P3) the audit itself says need production data that doesn't exist yet. I'm giving P0 full implementation detail (it's genuinely buildable now), P1 real architecture-level plans, and P2/P3 honest scoping — for P3 specifically, a concrete step-by-step plan would be fake precision, so I'm saying what the real first step is instead of inventing one.

---

## Build order

Following the audit's own §27 recommendation, corrected for what my research changed:

1. **P0-0** — RLS enforcement (security, do first, blocks nothing else but nothing should ship on top of a false isolation guarantee)
2. **P0-A, P0-C** — low-risk, additive, parallelizable
3. **P0-B** — now a real build (not just a check), sequence with P0-A/C
4. **P1-C** — memory↔code links (seeds the moat, ship early so usage data starts accumulating)
5. **P1-A, P1-B** — rerank + standing-answer cache
6. **P1-E** — first published benchmark (run once P1-A/B are in, so the number reflects the improved system)
7. **P1-D** — incremental indexing
8. **P2-A..D** — differentiators, after the foundation is solid
9. **P3-A..C** — see the honest note in that section before treating these as schedulable

---

## P0-0: Make RLS actually enforce

**Problem** (independently verified against current HEAD, not just relayed from `audit-results/`): no file under `database/` sets `FORCE ROW LEVEL SECURITY`. Both `deploy/compose/docker-compose.yml` and `deploy/native/postgres-setup.sh` connect every one of the 22 DB-backed services (via the shared `DATABASE_URL`/`SCOS_DATABASE_URL` env var, consumed through `packages/database/pool.go`) as the single `POSTGRES_USER` (`scos`), which **owns** every table. Postgres exempts table owners from RLS regardless of what policies exist, unless `FORCE ROW LEVEL SECURITY` is set. The only `NOSUPERUSER NOBYPASSRLS` role anywhere in the repo is a throwaway fixture in `database/integration/rls_test.go` (`it_role_<random>`), never wired into a real deployment.

**A gap the audit missed, found in this pass:** `database/migrations/000003_control_plane.sql` enables RLS on `capabilities` (line 74) but never adds a policy for it — the file adds policies for the other 3 tables it enables (`lifecycle_stages`, `routing_decisions`, `outbox_dead_letters`) but not `capabilities`. `capabilities` has no `tenant_id` column — it's shared reference data (a capability registry read/written by `agent-registry`, `autonomous-control-plane`, `compatibility-gateway`, `extension-manager`, `mcp-gateway`, `code-intelligence`). If `FORCE ROW LEVEL SECURITY` goes on this table without a policy, it becomes **deny-all** under the new restricted role — capability registration breaks across six services on day one. Every other RLS-enabled table in the repo (confirmed by reading all 9 `database/policies/*.sql` files plus every `ENABLE ROW LEVEL SECURITY` in `database/migrations/*.sql`) already has a matching policy — `capabilities` is the one real hole.

**Design:**

1. **New role, `scos_app`** — `NOSUPERUSER NOBYPASSRLS`, distinct from the migration-owning `scos` role. Services connect as `scos_app`; migrations keep running as `scos` (which must own tables to `ALTER`/`FORCE` them, and to create `scos_app` in the first place).
2. **Role creation goes in a new versioned SQL file, not a shell heredoc** — consistent with how every other schema change in this repo is managed (`scripts/scos-migrate` applies `database/migrations/*.sql` then `database/policies/*.sql` in filename order, tracking applied versions in `scos_schema_migrations`). New file: `database/policies/000011_app_role.sql`:
   - `CREATE ROLE scos_app LOGIN PASSWORD :'app_password' NOSUPERUSER NOBYPASSRLS` (idempotent via a `DO $$ ... IF NOT EXISTS ... ELSE ALTER ROLE ... END $$` block, mirroring the exact pattern already in `deploy/native/postgres-setup.sh`).
   - `GRANT USAGE ON SCHEMA public TO scos_app;`
   - `GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO scos_app;` and `GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO scos_app;` — the exact minimal-DML pattern already proven in `database/integration/rls_test.go:216-223`, generalized from the test schema to `public` (confirmed: no `CREATE SCHEMA` anywhere in `database/migrations/`, so `public` is the only schema in play; one `SERIAL` sequence exists, `000017_transparency_event_sequence.sql`, hence the sequence grant).
   - `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO scos_app;` — without this, every future migration that adds a table silently leaves `scos_app` unable to touch it until someone remembers to grant it by hand. This is the safeguard against P0-0 quietly regressing. (As implemented, deliberately omits `FOR ROLE <name>` so it applies to whichever role actually runs migrations, rather than hardcoding `scos` and breaking on a renamed owner.)
3. **`:'app_password'` needs threading through `scripts/scos-migrate`** — a required env var `SCOS_APP_PASSWORD`, checked at the top of the script the same way `SCOS_DATABASE_URL` already is, passed to every `psql_cmd` invocation via `-v app_password="$SCOS_APP_PASSWORD"` (psql's `:'varname'` quoted-literal substitution). Harmless for files that don't reference it.
4. **New policy for `capabilities`**: `database/policies/000010_capabilities_rls.sql`, mirroring the exact pattern already used for the same "shared, non-tenant, authenticated-principal-gated" shape in `database/migrations/000019_okf_governance_lifecycle.sql:52` (`okf_governance_roles_authenticated_access`):
   ```sql
   CREATE POLICY capabilities_authenticated_access ON capabilities
       FOR ALL USING (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL)
       WITH CHECK (NULLIF(current_setting('scos.principal_id', true), '') IS NOT NULL);
   ```
   This is a new security policy, not plumbing — flagging it as such. It grants any authenticated service-context principal full access to the capability registry, matching the existing precedent for `okf_bundles`/`okf_concepts`/`okf_governance_roles`/`okf_proposals` (all non-tenant shared data gated the same way). If you want something narrower (e.g. write restricted to specific services), say so — I went with the established in-repo pattern rather than inventing a new access shape.
   File numbered `000010` (next after `000009_hierarchy_nodes_rls.sql`); `000011_app_role.sql` for the role comes after it so the role's grants apply to a schema whose policies are already complete.
5. **`FORCE ROW LEVEL SECURITY`** — `database/policies/000012_force_rls.sql`, one `ALTER TABLE <t> FORCE ROW LEVEL SECURITY;` per table, for the complete, verified set (every table with `ENABLE ROW LEVEL SECURITY` across all of `database/policies/*.sql` and `database/migrations/*.sql`, deduplicated — cross-checked, exact 1:1 match, 70 tables each side):

   `turns, events, operations, outbox_jobs, orchestration_runs, routing_decisions, message_chunks, checkpoints, idempotency_keys, memory_evidence, memory_relations, memory_embeddings, memory_candidates, memory_reviews, object_references, retention_rules, audit_entries, service_state, extension_manager_state, entities, entity_aliases, entity_representations, beliefs, belief_evidence, diary_entries, plugin_packages, plugin_installations, skills, skill_executions, hooks, hook_subscriptions, hook_executions, agent_adapters, adapter_instances, capability_graph, transparency_projections, dashboard_stream_cursors, recovery_runs, recovery_events, recovery_leases, tenants, principals, agents, projects, workspaces, agent_registrations, agent_handoffs, tool_definitions, context_envelopes, security_tokens, secret_references, hierarchy_nodes, namespaces, sessions, messages, memory_items, memory_versions, tool_executions, okf_bundles, okf_concepts, retrieval_runs, retrieval_results, capabilities, lifecycle_stages, outbox_dead_letters, okf_proposals, okf_proposal_transitions, okf_governance_roles, credentials, credential_acl`

   (`namespace_memberships` — the ACL table nearly every policy subqueries — deliberately has **no** RLS at all, confirmed, and stays that way: it's the table every other table's policy needs to read freely to evaluate membership; this is correct as-is and matches the design, not a gap to fix here. This is the same table the comparative audit's F-09 flagged as "without RLS" — worth noting for whoever eventually decides whether it should itself be tenant-scoped, but out of scope for P0-0, which is about enforcement, not coverage.)

6. **Deploy config changes:**
   - `deploy/compose/docker-compose.yml`: added `SCOS_APP_PASSWORD` to the `migrations` service; kept `migrations` on `SCOS_DATABASE_URL` (the `scos`-owner connection); every application service's `DATABASE_URL` (or, for the two Python workers that used the `SCOS_DATABASE_URL` key name directly, that key's value) now points at a new `SCOS_APP_DATABASE_URL` env var instead — 21 service blocks changed, `migrations` and `backup-manager` deliberately untouched (`backup-manager` already has its own independent `SCOS_BACKUP_DATABASE_URL`/`SCOS_BACKUP_RESTORE_DATABASE_URL` credential path and legitimately needs owner-level access for a full logical backup). `postgres`'s bootstrap `POSTGRES_USER` is a superuser in the official image, so it's unaffected by `FORCE ROW LEVEL SECURITY`.
   - `deploy/native/postgres-setup.sh`: the native `scos` role is **not** a superuser (only DB owner) and had no `CREATEROLE` — it cannot create `scos_app` as-is. Added `CREATEROLE` to the `CREATE ROLE`/`ALTER ROLE` block. Also emits `SCOS_APP_PASSWORD`/`SCOS_APP_DATABASE_URL` setup instructions in the script's final output, alongside the existing `SCOS_DATABASE_URL`/`DATABASE_URL` lines, with an explicit note that every service unit (not the migrations unit) must use `SCOS_APP_DATABASE_URL`.
   - `.env.example` documents both new vars with a short rationale comment.
7. **Deployment-time assertion** — `packages/database/rls_guard.go`, wired into `packages/database.NewPool` (not opt-in, since `NewPool` is Go-only and the shell-script migration runner never calls it — every legitimate caller of `NewPool` is a request-serving service that must be subject to RLS). Queries `SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user` on every pool open and fails closed if true.
8. **ADR** — `docs/adr/0012-rls-enforcement.md`, in the existing Context/Decision/Consequences/Compatibility/Migration/Rollback format (cross-references ADR-0001, whose implicit assumption this fixes).

**Explicitly out of scope for P0-0** (noted so it isn't silently folded in): the duplicate `000008` policy-file numbering bug (F-15, cosmetic, unrelated); giving `namespace_memberships` its own RLS policy (a real but separate design question — would need its own access model, not a byproduct of enforcement).

**Files touched:** `database/policies/000010_capabilities_rls.sql` (new), `database/policies/000011_app_role.sql` (new), `database/policies/000012_force_rls.sql` (new), `scripts/scos-migrate`, `deploy/compose/docker-compose.yml`, `deploy/native/postgres-setup.sh`, `.env.example`, `packages/database/pool.go`, `packages/database/rls_guard.go` (new), `docs/adr/0012-rls-enforcement.md` (new).

**Rollback:** Compose's `scos` is a superuser, so it's never subject to `FORCE ROW LEVEL SECURITY` — reverting is just pointing `DATABASE_URL` back at `SCOS_DATABASE_URL` and redeploying, no SQL to undo. Native's `scos` is not a superuser, so once `FORCE ROW LEVEL SECURITY` is live it's subject to policies too; the break-glass there is `ALTER ROLE scos BYPASSRLS;` (one statement, instantly restores full access for operators, revert with `NOBYPASSRLS` once fixed) rather than un-forcing RLS table-by-table.

**Verification — not yet run:** `database/integration/rls_deployed_role_test.go` (new) is written and ready — it checks `scos_app`'s attributes, `FORCE ROW LEVEL SECURITY` on a representative table sample including `capabilities`, that `capabilities` is actually readable under an authenticated principal (the regression this whole fix is about), and that no identity context means zero visible rows. It needs `DATABASE_URL` + `SCOS_APP_DATABASE_URL` set against a live Compose stack to run — `go test -tags=integration ./database/integration/... -run DeployedAppRole`. Also run `make failure-injection` after deploying (58 existing cases against a live stack).

---

## P0-A: Graph-centrality code context ranking (PageRank over `code_edges`)

**Problem:** `context-optimisation` and `code-intelligence` allocate context by content-class/token-budget, not by which symbols are actually structurally central to the query. Aider's `repomap.py` gets more retrieval value from a PageRank pass over a reference graph than SCOS's much larger retrieval stack gets from RRF-plus-linear-boost, at a fraction of the code.

**Confirmed in this pass:** no graph algorithm library exists anywhere in the repo (`go.mod` has none; no `workers/` Python service has `networkx`). This has to be added, not just wired up.

**Design:** implement in Go, inside `services/code-intelligence`, not as a new Python worker — the existing 3-tier extraction pipeline, `code_edges` storage, and `ChangeImpact` BFS already live there, and code-intelligence has no Python component today (it's the one plane that's pure Go); adding a cross-service hop for a ranking pass that only needs the edge table already in the same database would be unjustified latency and a new deployment unit for no real benefit.

- Add `gonum.org/v1/gonum/graph` (specifically `graph/network` for `PageRank`) to `go.mod`.
- New function in `pgstore.go` (alongside `ChangeImpact`): `RankSymbols(ctx, snapshotID string, personalize map[string]float64, maxResults int) ([]RankedSymbol, error)` — loads `code_edges` for the active snapshot into a `gonum` directed graph, runs `network.PageRank`, applies the personalization vector (weight toward symbols in `personalize`, mirroring Aider's `pers_args` — populate this from recently-changed files (via `code_file_versions`' timestamps) and explicitly-mentioned symbols the caller passes in).
- Expose as a new field on the existing `QueryByPathOrFQN`/`ChangeImpact` response (rank score alongside each result) rather than a wholly new endpoint first — this makes it immediately useful to `scos_code_query`/`scos_code_impact` without a new MCP tool. A dedicated `scos_code_rank` tool can follow once the field proves useful.

**Tests:** rank-order regression test against a small fixed repo fixture (a handful of files with known call relationships, assert the expected centrality ordering) — same style as existing `pgstore` tests.

**Complexity:** S. **Risk:** Low (purely additive, new field, no existing behavior changes). **Prerequisites:** none.

---

## P0-B: Write-time PII/secret scanning

**Problem, corrected from the audit's framing:** the audit listed this as "verify first, might already exist." It doesn't. `security-gate`'s `classifyContent` (`main.go:601-613`) is a literal-keyword check (`"secret"`, `"password"`, `"ssn"`, etc.) exposed only as an opt-in `POST /v1/permissions/classify` endpoint — nothing calls it from a write path, and it does no pattern-matching against actual secret *shapes* (AWS key format, JWTs, SSN digit patterns) or PII. This is a real build, at roughly the complexity the audit estimated for the "if needed" branch (S), not the XS "just check" branch.

**Design:**
- New package `packages/secret-scan` (mirrors the existing shared-package convention — `packages/database`, `packages/auth`, etc.): a small pattern set (regex for common secret shapes: AWS access keys, generic high-entropy tokens, JWTs, private key PEM headers, SSN-shaped digit sequences, credit-card-shaped digit sequences) plus a `Scan(content string) []Finding{Kind, Span, Redacted string}`. The README's mention of `gitleaks` in CI is a config/pattern-list precedent to borrow the *pattern shapes* from (gitleaks' own default `gitleaks.toml` regexes are a reasonable starting pattern set — MIT-licensed, safe to adapt), not code to import (gitleaks is a CLI tool, not a Go library meant for embedding).
- Wire it into the one real write chokepoint for conversational content: `memory-intelligence`'s `insertFact` path (`store.go`), before the row is written — on a match, either reject the write with a structured error (fail closed) or redact-and-flag (store the redacted form, mark `sensitivity` elevated, surface a warning) — **this is a product decision, not an engineering one: fail-closed protects better but breaks the write; redact-and-store is more forgiving but requires the caller to handle a "your input was modified" signal.** Recommend redact-and-flag by default (matches the failure-injection philosophy already in this repo of failing loud, not failing silent) with a per-namespace policy toggle for fail-closed, but this needs a decision before implementation, not just before ship.
- Consider extending the same scan to `okf-synchroniser`'s promotion write path (OKF documents are meant to be broadly readable/shared — a leaked secret there is worse, not better, than in raw conversational memory).

**Complexity:** S. **Risk:** Low technically, Medium on the product-decision axis (fail-closed vs. redact-and-flag needs a call before coding starts). **Impact:** High — this is a safety gap, not a feature gap, now that it's confirmed absent.

---

## P0-C: Same-key-different-value contradiction check

**Problem, corrected from the audit's framing:** the audit assumed `fact_key_norm`/`fact_value_norm` already exist on the table backing `Fact` and just need wiring into `isContradictory`. **They don't exist at all** — confirmed by reading `insertFact` (`store.go:91-153`) and the `memory_items`/`memory_versions` schema (`000001_initial.sql`) in full. `Fact` has a single free-text `Content` field; there is no key/value decomposition anywhere in the write path. This re-scopes P0-C from "wire up an existing column" to "add a normalization step that doesn't exist yet" — genuinely still small, but not the XS the audit estimated.

**Design:**
- Add two nullable columns to `memory_items` (a small additive migration, `000030_fact_key_value_norm.sql`, next in sequence after `000029`): `key_norm text`, `value_norm text`.
- Populate them in `insertFact` via a lightweight, deterministic extraction — not a new LLM call (would defeat the point of keeping this cheap): a simple "subject/predicate ≈ value" split is not reliably derivable from arbitrary free text without NLP, so the honest options are (a) only populate `key_norm`/`value_norm` when the fact already carries structured hints (check whether callers into `insertFact` ever pass anything more structured than `fact.Content` — if `maybePromoteToOKF`'s LLM classification path already extracts anything key-shaped, reuse that instead of building new extraction), or (b) accept a coarser normalization (lowercase, strip stopwords, take the first N significant words as `key_norm`, the rest as `value_norm`) that will under-perform on complex sentences but still catches the concrete case the audit cited ("ships on Tuesdays" vs. "ships on Thursdays" — same leading words, different trailing value). Recommend (b) to start, explicitly labeled as a heuristic with known limits, same spirit as the existing `isContradictory`/`findMergeCandidate` heuristics it sits next to.
- Extend `isContradictory` (`main.go:2156`) with a second check: same `MemoryNetwork`+scope (existing gate) AND `key_norm` equality AND `value_norm` inequality → contradictory, independent of the existing negation-word path (this catches same-polarity conflicts the negation check structurally cannot).

**Complexity:** S (revised up from the audit's XS-S, since normalization has to be built, not just read). **Risk:** Low. **Prerequisites:** decide extraction approach (a) vs (b) above before coding.

---

## P1-A: Cross-encoder rerank stage

**Design:** add an optional post-RRF stage to `retrieval-orchestrator`'s `rerankResults` (`main.go:832`), gated the same way vector search already gates on embedding-service availability (explicit fallback to the existing linear boost, never a silent skip). Serve a small local cross-encoder (e.g. `cross-encoder/ms-marco-MiniLM-L-6-v2`, consistent with the existing local-first `bge-small-en-v1.5` embeddings philosophy) — extend `workers/embeddings` (it already runs `onnxruntime` locally; a cross-encoder is the same inference shape, a second small ONNX model rather than a new service) with a `/rerank` endpoint taking `(query, candidates[])` and returning relevance scores. `retrieval-orchestrator` calls it after fusion, before the existing linear boost, with the same availability-gated fallback pattern.

**Complexity:** M. **Risk:** Low (additive, fallback-safe). **Prerequisites:** confirm `workers/embeddings`' `onnxruntime` setup can host a second model without a resource-budget conflict (check its current memory/CPU allocation in compose before assuming headroom).

---

## P1-B: Standing-answer cache

**Design:** exactly the schema the audit proposed (§23), which I've reviewed and see no issue with:
```sql
CREATE TABLE memory_standing_answers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    namespace_id uuid NOT NULL,
    question_key text NOT NULL,
    answer text NOT NULL,
    evidence_fact_ids uuid[] NOT NULL DEFAULT '{}',
    confidence float NOT NULL DEFAULT 0.5,
    last_refreshed_at timestamptz NOT NULL DEFAULT now(),
    refresh_reason text,
    UNIQUE (tenant_id, namespace_id, question_key)
);
```
RLS on this table follows the exact tenant+namespace-membership pattern in `database/policies/000003_data_plane_rls.sql` — and **must** get `FORCE ROW LEVEL SECURITY` and a real policy in the same migration that creates it, per P0-0's now-established convention (don't repeat the `capabilities` mistake on a brand-new table). `retrieval-orchestrator`'s `handleRetrieve` checks this table first (namespace + question-key match) before the 5-source fan-out; a background refresh job (extend `workers/index-maintenance`, which already exists for this class of job, rather than adding a new worker) rewrites rows when relevant facts change.

**Complexity:** M. **Risk:** Low. **Prerequisites:** define the initial `question_key` set (the audit doesn't specify one — needs a short list of the highest-frequency questions worth precomputing before this is buildable, e.g. "user_preferences_summary", "project_architecture_summary").

---

## P1-C: Explicit memory↔code evidence links

**Design:** the audit's schema (§23), reviewed, no issues:
```sql
CREATE TABLE memory_code_links (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    fact_id uuid NOT NULL,
    code_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id),
    link_type text NOT NULL CHECK (link_type IN ('evidences','motivated_by','invalidated_by')),
    confidence float NOT NULL DEFAULT 1.0,
    source text NOT NULL CHECK (source IN ('explicit','okf_promotion','inferred')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (fact_id, code_symbol_version_id, link_type)
);
```
Two write points, both real and shippable now: (1) a new MCP tool `scos_link_evidence(fact_id, code_symbol_version_id, link_type)` an agent calls explicitly when it knows a decision and a code change are related — zero inference logic required; (2) `maybePromoteToOKF` (`memory-intelligence/main.go:789`), when the promoted content already cites a specific symbol/change-impact result in its context, writes a `source='okf_promotion'` link automatically. `retrieval-orchestrator` gets a new retrieval mode that joins `memory_code_links` alongside the existing 5-source fan-out when a query is code-and-memory-shaped. Again: `FORCE ROW LEVEL SECURITY` + real policy from day one on this table.

**Complexity:** M. **Risk:** Low. **Impact:** seeds the whole §25 moat — sequence this early even though it's P1, not after P1-A/B, per the audit's own §27.

---

## P1-D: Content-hash-gated incremental code indexing

**Confirmed in this pass:** `IndexSnapshot` (`pgstore.go:149`) writes every file in the input list unconditionally — no comparison against the parent snapshot's `code_file_versions.content_hash` before writing, despite that column already existing and being populated (`000025_code_intelligence_persistence.sql:97`).

**Design:** before `indexFileLocked` writes a new `code_file_versions` row for a file, look up the parent snapshot's row for the same `stable_key`/path and compare `content_hash`; if unchanged, skip re-parsing entirely and instead insert a `code_file_versions` row that just references the prior snapshot's symbol/edge rows (a cheap pointer-copy, not a re-derivation) rather than doing the full extraction pipeline again. This is a driver-logic change inside `IndexSnapshot`/`indexFileLocked`, not a schema change — the schema already supports it.

**Complexity:** M. **Risk:** Medium (touches the core indexing write path — needs solid test coverage on the "what exactly gets copied vs. re-derived" boundary before this ships, since a bug here would silently produce stale symbol data that looks fresh). **Prerequisites:** trace every caller of `IndexSnapshot` first (this pass found the one HTTP handler at `pghandlers.go:191`, but confirm there isn't a second caller, e.g. from an internal reindex job, that assumes today's always-full-rewrite behavior).

---

## P1-E: First ADR-0010-compliant published benchmark

**Design:** build the two corpora ADR-0010 already mandates the process for: (a) a LongMemEval-style long-horizon memory recall set — same public benchmark Hindsight cites, making the comparison directly meaningful; (b) a change-impact-accuracy set mined from SCOS's own commit history (does `ChangeImpact` correctly predict what a real historical commit touched downstream?) — uniquely available to SCOS since it has real git history to mine ground truth from, unlike a synthetic benchmark. Run via the existing `packages/benchmark-harness` (`ComputeRecallAtK`/`ComputePrecisionAtK`/`ComputeMRR`/`ComputeNDCGAtK` already implemented) against: an unoptimized baseline, the prior production version, and the strongest lawfully-evaluable competitor, per ADR-0010's own requirement. Publish the result.

**Complexity:** M-L (corpus-building is the real work, harness already exists). **Risk:** Low. **Sequencing:** run this after P1-A/B ship, so the published number reflects the improved system rather than needing an immediate re-run.

---

## P2 items — differentiators (architecture-level, not implementation-ready)

These are real and worth doing, but each is genuinely multi-week and none blocks anything above. Giving each a full step-by-step plan now would be premature — the right next step for each is a short design spike, not a build ticket.

- **P2-A, ontology-guided extraction** (Cognee-inspired): anchor a fixed entity-type vocabulary on SCOS's existing typed `MemoryNetwork`s. First real step: enumerate the entity types actually worth constraining (service, host, decision, person, etc.) from real usage data, not upfront guessing — extraction-quality regression risk (the audit's own risk flag) is exactly the failure mode a fixed vocabulary chosen too early produces.
- **P2-B, bitemporal schema extension**: add `belief_expired_at` (system-time: when SCOS stopped trusting a fact) distinct from `valid_until` (world-time: when the fact stopped being true), matching Graphiti's 4-timestamp model. Schema-only + a `createOrSupersedeFact` change — smallest P2 item, could realistically move up in priority if you want one differentiator shipped sooner.
- **P2-C, pluggable retrieval strategies**: let the caller pick a retrieval strategy per query-type instead of always running the fixed 5-source fan-out (Cognee's model). This is an API-surface change (Medium risk, per the audit) — needs a design pass on what strategies actually exist before touching `retrieval-orchestrator`'s handler shape.
- **P2-D, agent-execution-trace consolidation** (Cognee's `cognify_agent_trace_feedback.py`-inspired): worth prioritizing within P2 specifically because it's closer to SCOS's actual audience (coding agents, not casual chat) than the other three — but needs `workers/consolidation`'s `synthesis.py`/`llm_enrichment.py` read in full first (this pass didn't go past module-structure level on that pipeline) before a real design is possible.

---

## P3 items — the honest version

The audit itself flags these as research-grade or data-dependent, and I'm not going to manufacture a fake implementation plan for any of them. Here's what's actually true about each:

- **P3-A, automatic (inferred) memory↔code linking**: cannot be scoped into concrete steps until P1-C (explicit links) has shipped and accumulated real linked pairs to learn from — there's no inference target to build against yet. Real first step, once P1-C has data: look at what the explicit links actually look like and whether a simple co-occurrence heuristic (a fact and a code change touched in the same session) gets reasonable precision before reaching for anything ML-shaped.
- **P3-B, surprisal-driven consolidation scheduling**: the audit is explicit that this must be *reimplemented from Honcho's idea*, not ported (Honcho is AGPL-3.0, SCOS is MIT). This is a genuine research effort — an information-theoretic "how much does this observation change existing belief" signal isn't a known-shape engineering task. Not schedulable as a normal backlog item; if you want to pursue it, the right unit of work is a research spike with an explicit "we might not find something that works" exit condition, not a sprint ticket.
- **P3-C, cross-repository code graph**: this is as much a product question as an engineering one (the audit says so directly — valuable mainly for multi-repo adopters, which may not be near-term SCOS market). Don't build this speculatively; decide whether multi-repo is actually a target use case first.

---

## Verification (applies across the whole plan)

- **P0-0** is the one item with a concrete, runnable verification path today: run `database/integration/rls_deployed_role_test.go` against a live Compose stack, then `make failure-injection` post-deploy, then confirm capability registration still works.
- **P0-A/B/C**: each gets its own unit/regression test as described inline above; none require new infrastructure to test.
- **P1 items**: each new table gets RLS from day one (test via the same `rls_test.go` pattern); P1-A/B need a before/after retrieval-quality comparison, which is exactly what P1-E's benchmark harness is for — worth running a quick harness pass on P1-A/B specifically even before the full P1-E corpus effort, since the tooling already exists.
- Nothing in this plan should ship without `make audit` (the governance gate) passing, per this repo's own convention.
