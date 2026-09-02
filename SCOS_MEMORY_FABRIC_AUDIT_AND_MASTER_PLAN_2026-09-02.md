# SCOS Memory — Competitive Audit and Master Plan

**Repository audited:** `dylmarriner/scos-memory` (local working tree, folder `scos-memory-fabric`)
**HEAD at audit time:** commit `67e2e59` ("docs: add SCOS memory competitive audit and master plan") **plus 18 uncommitted modified files** on top of it — the code-intelligence rewrite (SCIP ingestion, Tree-sitter, Go-native extraction, pgstore, graphify) is mid-flight, not a clean snapshot.
**Audit date:** 2026-09-01 (Pacific/Auckland)
**Supersedes:** the same-day `SCOS_MEMORY_COMPETITIVE_AUDIT_AND_MASTER_PLAN.md` / `SCOS_MEMORY_TECHNICAL_BLUEPRINT_AND_ENGINEERING_ROADMAP.md` already committed to this repo, which were pinned to `78755c40b2` (2026-07-22) — a revision that predates the current code-intelligence rewrite. This document is an independent, from-scratch pass, at the user's explicit request, against current HEAD including the in-flight changes.
**Evidence classes** (used throughout, per line): **Verified** = I read the actual source in this pass. **Documented** = stated in the project's own README/docs, not traced into source in this pass. **Inferred** = implied by architecture but not directly confirmed. **Unknown** = insufficient evidence either way. Nothing below is asserted without one of these tags where it matters.

> **Correction added after initial publication (same day).** This document originally rated SCOS's RLS-based multi-tenant isolation "Strong" and listed it as SCOS's clearest differentiator over every competitor inspected. That was wrong as a statement about the deployed system. `audit-results/2026-09-02/` and `audit-results/comparative-divergence-audit.md` — a separate, independently-run hostile-security-audit process already in this repo — record that no migration or policy file sets `FORCE ROW LEVEL SECURITY`, and both deployment paths (`deploy/compose/docker-compose.yml`'s `POSTGRES_USER`, `deploy/native/postgres-setup.sh`) connect services as the table-owning Postgres role, which Postgres exempts from RLS regardless of whether policies exist. I verified this directly against current HEAD: no `FORCE ROW LEVEL SECURITY` anywhere in `database/`, and the only role created with `NOSUPERUSER NOBYPASSRLS` (the restriction needed for RLS to actually bind) is in `database/integration/rls_test.go`, never wired into an actual deployment. **The RLS policies are correctly written and correctly scoped (verified, §2.2) but not currently enforced against the role that actually connects.** Every claim below rating this "Strong" has been corrected to reflect that; see §2.2, §7, §5, §18.

> **Honesty note on scope.** The originating request asks for forensic, file-and-line-level verification of ~15 named competitors across 27 sections — genuinely a multi-week, multi-person research effort. I cloned and read source for **seven** competitors (Mem0, Graphiti/Zep, Honcho, Cognee, Letta/Letta-Code, Sourcegraph SCIP, Aider) plus named Hindsight from its own README (not yet source-verified beyond that). That is real, current (all repos cloned today, 2026-09-01, at their live HEAD), and enough to ground every major claim below in either code or the vendor's own words — not enough to claim forensic-file-and-line coverage of every named competitor (Zep-the-product vs. Graphiti-the-library, LangMem, OpenMemory, Joern were not independently source-verified in this pass and are marked accordingly where they appear). Where I don't know, I say Unknown rather than fill the gap with confident-sounding prose.

---

## 1. Executive Summary

SCOS Memory is not a memory SDK wrapped in a demo. At current HEAD it is a **23-service, ~50,000-line Go platform** (plus ~4,000 lines of Python workers) with PostgreSQL as the single authoritative store, row-level security enforced at the database layer on every data-plane table, an append-only event/turn journal, a git-backed curated-knowledge store (OKF), and a 43-section, RFC-2119-style governing specification (`GOVERNANCE.md`, 113KB) that a build-time gate (`make audit` → `scripts/audit_governance.py`) actually checks the running system against. That combination — a written spec, enforced in CI, against a live system — is not something any of the seven competitors inspected in this pass do.

The most important finding of this pass: **the previous audit's headline verdict is already partly obsolete by the project's own subsequent work.** That audit (committed the same day, pinned to `78755c40b2`, 2026-07-22) concluded code-intelligence was "a JSON-file toy store... far behind Sourcegraph/Joern" and listed seven P0s. Commits since then — `1356ccd` (real Go AST symbol extraction via `go/parser`), `b9a9c1c` (real syntactic call-edge extraction), `35c5d3c` (durable snapshot-based store replacing the in-memory map), `64c63b7` (SCIP ingestion), `1898f04` (Tree-sitter fallback for JS/TS/Python/Rust), `059ba02` (real BFS change-impact analysis) — have already shipped a durable, immutable-snapshot, rename-lineage-aware, three-tier (Go-native AST → Tree-sitter → SCIP) symbol store with a working `code_edges`-based blast-radius query. That is genuinely competitive code-intelligence architecture, not a toy. It is not yet *complete* (no code embeddings/dense semantic code search — deliberately deferred per the migration's own comment; no cross-repository graph; single-hop-per-language call resolution with no type-checking), but "JSON file, far behind everyone" is no longer an accurate description.

Where SCOS is genuinely behind, verified against real source in this pass:

- **Consolidation sophistication.** SCOS's `isContradictory` is a shared-negation-word-plus-shared-term-count heuristic (`services/memory-intelligence/cmd/server/main.go:2156`). Honcho's `dreamer` and Hindsight's `reflect`/`observations` pipeline do LLM-driven, evidence-tracked belief refinement with proof counts; SCOS's `findMergeCandidate` only calls an LLM at all above a 0.5 heuristic-overlap shortlist, and falls back to a blunt 0.85-overlap deterministic rule with no LLM configured.
- **Retrieval breadth per call.** SCOS's `retrieval-orchestrator` fuses exact + full-text + vector + one-hop relation expansion + recent session state via RRF, then applies a **deterministic, non-learned** rerank (`rerankResults`, `services/retrieval-orchestrator/cmd/server/main.go:832`: multiply by `1 + 0.15*authority + 0.15*confidence`). Hindsight documents a cross-encoder reranking model after RRF; nobody in SCOS's stack does a cross-encoder or LLM rerank pass.
- **Zero precomputed/standing answers.** Hindsight's "mental models" (a standing, continuously-rewritten answer to a fixed question, read straight from a DB row with **no retrieval and no LLM call at read time**) has no SCOS equivalent. Every SCOS read pays full retrieval cost.
- **Integration and DX surface.** Hindsight ships 50+ named framework/agent integrations, a hosted benchmark site, a Helm chart, and works with five different "bring your own subscription" auth modes. SCOS integrates 7 agent runtimes through one MCP surface and one Claude plugin — real, but an order of magnitude smaller surface, and with no independent, reproducible, publicly-hosted benchmark result (SCOS's own ADR-0010 explicitly *requires* one before any superiority claim, and none has been run yet against an external system).
- **No dense/semantic code search.** Migration `000025`'s own header defers `code_embeddings` to "a separate phase (P0-008, P1-015)." Code retrieval today is exact path/FQN lookup and Postgres full-text search only — no vector search over code, unlike memory retrieval which already has pgvector.

Where SCOS is genuinely ahead of everything inspected, verified in this pass:

- **RLS policy design (not, as deployed, RLS enforcement — corrected).** `database/policies/000003_data_plane_rls.sql` enables RLS on every journal/memory/review/audit table and scopes every policy through `namespace_memberships` (`can_read`/`can_write` grants) resolved from `current_setting('scos.tenant_id')`/`principal_id` — a genuinely better-designed policy layer than any application-side filter (Mem0/Cognee filter by `user_id`/`agent_id` in code; Honcho scopes by workspace/peer in its ORM, also application-side). **But as currently deployed this is not an enforced advantage**: no migration sets `FORCE ROW LEVEL SECURITY`, and both deployment paths connect as the table-owning role, which Postgres exempts from RLS regardless. The only correctly-restricted role (`NOSUPERUSER NOBYPASSRLS`) exists in `database/integration/rls_test.go` only. Verified against current HEAD via this repo's own `audit-results/` hostile-security-audit findings, cross-checked directly rather than taken on faith. Until a non-owner, non-bypass role is what services actually connect as, and `FORCE ROW LEVEL SECURITY` is set, **treat isolation today as no stronger than application-layer discipline** — the same category SCOS's competitors are in, just with a better foundation to fix it from than they have.
- **A governance ADR that requires benchmark rigor before any superiority claim** (`docs/adr/0010-superiority-benchmark-methodology.md`): frozen corpora, `Recall@K`/`Precision@K`/`MRR`/`nDCG` via a shared `packages/benchmark-harness`, comparison against an unoptimized baseline *and* the prior production version *and* the strongest lawfully-evaluable competitor, explicit prohibition on cherry-picked demonstrations. This is stricter benchmark governance than any competitor's public repo documents (Hindsight publishes a live benchmark site but that is a marketing claim pipeline, not an internal gate on SCOS's own engineers).
- **A git-native curated-knowledge store with an explicit promotion gate distinct from raw memory.** OKF (`okf/`) is separate from the conversational record; promotion is LLM-classified with a deterministic-reject fallback (`classifyForOKF`, `main.go:888`) and produces reviewable Markdown proposals, not silent writes. Letta-Code's MemFS is the closest competitive idea (agent context, including memory blocks, tracked in git) — but MemFS tracks an *agent's own* editable context, not a separate reviewed-and-approved knowledge tier the way OKF does.
- **Local-first embeddings by default.** `workers/embeddings` runs `BAAI/bge-small-en-v1.5` (384-d) locally; no external embedding API call is required for the base retrieval path. Mem0 and most competitors default to a hosted embedding API (OpenAI) unless reconfigured.

**The single most important strategic risk:** SCOS's service count (23) and platform breadth are ahead of where its retrieval and consolidation *algorithms* currently are. Competitors with a fraction of the codebase (Aider's `repomap.py` is 867 lines) get more retrieval mileage out of one well-chosen technique (PageRank over a reference graph) than SCOS's much larger `retrieval-orchestrator` gets from RRF-plus-linear-boost. Breadth without matching algorithmic depth is a real liability, not a hedge — it means more surface to keep coherent for a comparatively smaller quality return per engineering hour.

---

## 2. Current scos-memory Architecture — Verified

### 2.1 Shape

Go 1.26 monorepo, MIT license, root `go.mod` at `github.com/dylmarriner/scos-memory`. 23 services under `services/`, each an independent Go HTTP binary under `cmd/server/`, plus 18 shared packages under `packages/` (auth, database, telemetry, `benchmark-harness`, `context-contracts`, `httpmid`, `tlsserve`, `llmclient`, `language-adapters`, `hook-sdk`, `skill-sdk`, `capability-registry`, `execution-adapters`, `routing-contracts`, `service-state`, `release`, `agent-adapter-sdk`, `hermes-memory-provider`). Non-test Go LOC: **50,181** (`find . -name '*.go' -not -name '*_test.go' | xargs wc -l`, this pass). Python workers: **~4,024 lines** across `consolidation`, `embeddings`, `index-maintenance`, `policy-evaluation`.

Services present at HEAD: `agent-registry`, `autonomous-control-plane`, `backup-manager`, `cloud-control-plane`, `code-intelligence`, `compatibility-gateway`, `context-optimisation`, `credential-vault`, `execution-proxy`, `extension-manager`, `graphify-provider`, `hermes-provider`, `mcp-gateway`, `memory-gateway`, `memory-intelligence`, `observability`, `okf-synchroniser`, `readiness`, `recovery-orchestrator`, `retrieval-orchestrator`, `security-gate`, `tool-governance`, `transparency-projection`.

Dependencies of note (`go.mod`): `jackc/pgx/v5` (Postgres driver), `santhosh-tekuri/jsonschema/v6`, `scip-code/scip` (SCIP protobuf bindings — Go), `tree-sitter/go-tree-sitter` + grammars for JavaScript, Python, Rust, TypeScript (no Go grammar — Go is parsed with the standard library's own `go/parser`/`go/ast`/`go/types`, not Tree-sitter). No embedding-model client library in `go.mod` — embedding is done by the separate Python `workers/embeddings` service.

### 2.2 Data plane: PostgreSQL is the only source of truth

29 sequential, append-only migrations (`database/migrations/000001`–`000029`), never destructively altered (verified by listing — no gaps, monotonically numbered). RLS is enabled and policy-scoped on every journal/memory/review/audit table (`database/policies/000003_data_plane_rls.sql`, verified): `message_chunks`, `checkpoints`, `idempotency_keys`, `memory_evidence`, `memory_relations`, `memory_embeddings`, `memory_candidates`, `memory_reviews`, `object_references`, `retention_rules`, `audit_entries`, `service_state`, `extension_manager_state`. Every policy resolves tenant/namespace membership through `current_setting('scos.tenant_id', true)` and a `namespace_memberships` join with explicit `can_read`/`can_write` booleans — a well-designed policy layer. **Correction:** this is not, as deployed, "a database-layer constraint an application bug can't bypass" — it's currently a database-layer constraint the *connecting role itself* bypasses, because no migration sets `FORCE ROW LEVEL SECURITY` and both deployment paths connect as the table-owning role, which Postgres exempts from RLS by default regardless of policies present. Verified against current HEAD via `audit-results/2026-09-02/` and `audit-results/comparative-divergence-audit.md` (a separate hostile-security-audit already in this repo), cross-checked directly: no `FORCE ROW LEVEL SECURITY` anywhere in `database/`, and the only `NOSUPERUSER NOBYPASSRLS` role exists in `database/integration/rls_test.go`, not in any deployment path. The policies are real and correctly scoped; enforcement against the actual runtime connection is not yet real.

OKF (`okf/`) is a parallel, git-versioned store for curated/approved knowledge (`architecture/`, `decisions/`, `glossary/`, `governance/`, `infrastructure/`, `procedures/`, `projects/`, `tools/`), explicitly separate from the raw conversational record — the README's stated split ("persist every interaction, promote only durable knowledge") is real, not aspirational: promotion is a distinct code path (§2.4 below), not implicit.

### 2.3 Memory-intelligence — Verified (`services/memory-intelligence/cmd/server/{main,store}.go`, 3,356 non-test LOC combined)

`engineServer` holds **no domain data in memory** — every read/write hits PostgreSQL (explicit doc comment on the type, confirmed by the handler bodies). Core types:

- **`Fact`**: id, `MemoryNetwork` (`world | experience | entity | belief | procedure | negative` — six typed memory networks, not one flat table), content, source (`explicit|observed|inferred|external`), authority level, confidence, scope, `observed_at`/`valid_from`/`valid_until`/`recorded_at` (temporal fields distinct from each other — event time vs. record time, though not full Graphiti-style bitemporal with a separate invalidation timestamp), `superseded_by`, status (`active|merged`), evidence list, `inferred` flag + `derivation`, revision counter, tenant/principal/namespace identity.
- **`Entity`**: name/type (user, agent, organisation), linked fact IDs, preferences, goals, free-form traits map, scope.
- **`Belief`**: premise → conclusion, confidence, evidence IDs, `revised_by` lineage.
- **`TemporalRelation`**: typed edges (`derived_from | contradicts | supersedes | evidence_for | used_in`) between facts.
- **`HierarchyNode`**: an explicit six-level organizational hierarchy — `palace → wing → drawer → closet → diary → artifact` (a memory-palace metaphor; notably, Letta-Code independently uses the same "palace" word for its own memory view, `/palace` — convergent naming, not shared code).

Mechanisms verified in code:

- **Contradiction detection** (`isContradictory`, `main.go:2156`): same network+scope required; requires exactly one side to contain a negation word (`not|never|no|isn't|doesn't|don't|won't|can't|without`) and at least one shared content word >3 chars. This is a cheap bag-of-words heuristic, not semantic — it will miss "the deploy runs on Tuesdays" vs. "the deploy runs on Thursdays" (no negation word, real contradiction) and will false-positive on unrelated negated sentences that happen to share a long word.
- **Duplicate/merge detection** (`findMergeCandidate`, `main.go:2261`): `overlapScore` (shared significant-word fraction, network/scope-gated) shortlists candidates ≥0.5; contradictory candidates are excluded up front (routed to correction instead); above the shortlist threshold, an LLM call decides `is_duplicate`/`richer` (bounded to top-3 candidates, 10s timeout); with no LLM configured, falls back to accepting only ≥0.85 heuristic overlap and keeping the longer text as canonical. This is deliberately bounded (LLM never runs on every write) but the deterministic fallback is coarse.
- **OKF auto-promotion** (`maybePromoteToOKF` → `classifyForOKF`, `main.go:789–950`): gated on authority/confidence thresholds *and* LLM availability; the LLM is asked to classify durability into one of eight OKF categories and explicitly instructed to reject "one-off observations, unverified inferred facts, temporary state, narrow personal preference, or ordinary conversational content" — rejection (`durable: false`) is the documented common case, not an error path. Output is rendered as YAML-frontmatter Markdown matching OKF's existing document shape and POSTed as a **reviewable proposal** to `okf-synchroniser`, not written directly.
- **`handleReflect`** (`main.go:1483`): given a list of evidence fact IDs, pairwise-checks contradictions among just those facts, drafts candidate beliefs from non-inferred evidence (confidence × 0.9), and flags existing beliefs whose conclusion text-overlaps the new evidence as `stale_beliefs`. This is a real reflection primitive but shallow — it's substring/prefix matching (`strings.Contains` on the first 30 chars) for staleness, not semantic.
- **Feedback** (`handleFactFeedback`, explicitly commented as mem0's `feedback()` equivalent): records `POSITIVE|NEGATIVE|VERY_NEGATIVE` + reason against a fact without acting on it — a raw quality signal, not (yet, per the code) fed back into ranking or confidence automatically.
- **Forgetting** (`forgetFact`): soft-delete (status=`revoked`), optional `cascade` to revoke the supersession chain, deletes access-tracking rows. Not a hard delete; not a TTL-driven background sweep (TTL field exists on `Fact` but no background expiry sweep was located in this pass — Unknown whether one exists elsewhere, e.g. in `workers/index-maintenance`).

### 2.4 Code-intelligence — Verified, and materially different from the pinned-revision audit (`services/code-intelligence/cmd/server/*.go`, 3,777 non-test LOC across `main.go`, `pgstore.go`, `pghandlers.go`, `scipimport.go`, `treesitter.go`, `goextract.go`)

Storage model (`database/migrations/000025_code_intelligence_persistence.sql`, verified): **immutable, append-only generations**. `code_repositories` holds a mutable `active_snapshot_id` pointer only; `code_repository_snapshots` is unique on `(repository_id, branch, commit_id, extractor_version, configuration_hash)` and never updated in place except to flip `active`→`superseded`. Symbol and file identity is **split from version**: `code_symbols`/`code_files` hold a stable conceptual identity (`stable_key`) independent of path; `code_symbol_versions`/`code_file_versions` hold what that identity looked like in one snapshot, linked to a prior identity via `renamed_from_symbol_id`/`renamed_from_file_id` when a rename is detected. The migration's own header names the two structural defects this fixes relative to the old in-memory store (path-hash identity destroying rename lineage; one mutable commit pointer destroying prior-commit state on reindex) and references the exact tests that pinned those defects (`TestBaselineRenameLosesSymbolLineage`, `TestBaselineReindexDestroysPriorCommitState`) — i.e., the fix was driven by a regression test written against the *old* behavior first.

Three extraction tiers, verified:

1. **Go-native** (`goextract.go`, 262 LOC): real `go/parser` AST walk, not filename convention. `CalledNames` are extracted syntactically per-declaration; `pgstore.go`'s `insertCallEdgesLocked` only materializes a `CALLS` edge when **exactly one** declaration in the same package/snapshot matches the called name, and silently drops ambiguous or unresolved-in-package calls rather than guessing. No `go/types` checking — this is AST-precise, not type-precise. Content hash is per-declaration span, not per-file, so editing one function doesn't change every other symbol's identity.
2. **Tree-sitter** (`treesitter.go`, 195 LOC): grammars for JavaScript, Python, Rust, TypeScript; per-language `callShape` config for how to recognize call expressions and callee selectors; same syntactic-names-only call extraction as the Go path.
3. **SCIP ingestion** (`scipimport.go`, 443 LOC): decodes real SCIP protobuf indexes (produced externally by `scip-go`, `scip-typescript`, `scip-python`, etc. — SCOS does not run a type checker itself, it consumes one's output). Symbol identity is the SCIP symbol string itself (stable across renames by construction, per SCIP's grammar), not a content hash. Ingests both declared symbols and reference occurrences, attributing each reference to its innermost enclosing definition. This is the precision tier: a real compiler/language-server-derived signal, when the caller supplies one.

Retrieval and analysis, verified: `QueryByPathOrFQN` (exact/prefix), `QueryLexical` (Postgres full-text search, `000026_code_lexical_search.sql`), `DiffSnapshot` (structural diff between two generations: added/removed/renamed/modified, with parent linkage), and **`ChangeImpact`** (`pgstore.go:670`): resolves a query to matched symbols in the active snapshot, then BFS-walks `code_edges` backwards (`callersOf`) up to `maxDepth` hops (default 3) to produce `DirectDependents` (one hop) and `TransitiveDependents` (further hops) — a real, if CALLS-only (no DATA_FLOW/READS/WRITES edges exist yet), blast-radius query grounded in actual extracted edges rather than the old codeStore's `OutboundRelations` field, which the prior heuristic extractor never populated at all.

**What is explicitly deferred**, per the migration's own comment: `code_chunks` exists (one row per symbol, feeding the existing query API) but **`code_embeddings`, a durable job queue, and impact-prediction training data are out of scope for this migration**, tracked as separate phases (`P0-008`, `P1-015`). **There is no dense/semantic code search today** — only exact/prefix symbol lookup and lexical FTS. This is the single largest remaining code-intelligence gap versus a Sourcegraph-class system (which layers embeddings-based "smart search" atop SCIP) or Cognee's multiple graph-completion retrievers.

### 2.5 Graphify-provider — Verified; the prior audit's flagged staleness defect is fixed

`services/graphify-provider/cmd/server/graphify.go` wraps an external `graphify` CLI (`exec.CommandContext`, arguments passed as a slice, never through a shell — verified no shell interpolation; `rejectFlagLike` refuses any argument beginning with `-`, closing an argument-injection vector even though there's no shell-injection vector to begin with). `ensureCachedGraph`'s cache key (`cacheKey`, hashed, not path-flattened — a documented prior collision bug is called out in comments) folds in **the repo's current content fingerprint**, the `graphify` binary's own version, and the backend flag — so any repo state change, even an uncommitted edit, misses the cache and triggers a fresh extraction. This directly resolves what the pinned-revision audit called a P0: "cached graph existence is checked without staleness invalidation." It is now content-fingerprint-invalidated, same principle as the code-intelligence store's content-hash keys. Old cache generations are not garbage-collected (documented as a known gap, not silently missing). Worker count is bounded to 1 and clustering disabled (`--max-workers 1 --no-cluster`) after an observed OOM kill against a real repo (named in comments: "paperclip") — an operational finding baked into the flags, not a guess.

### 2.6 Retrieval-orchestrator — Verified (`services/retrieval-orchestrator/cmd/server/main.go`, 1,099 LOC)

`handleRetrieve` runs, per query: exact match, full-text search (Postgres `to_tsvector`/`to_tsquery`), vector search (pgvector cosine distance, with **automatic fallback to FTS** if the embedding service or pgvector index is unavailable — not a silent degrade, an explicit fallback path), one-hop relation expansion over already-found candidates, and recent-session-state lookup. All five result sets are fused with **Reciprocal Rank Fusion** (`fuseResults`, configurable `k`), then deduplicated, then passed through a **deterministic, non-learned rerank** (`rerankResults`: `FusedScore *= 1 + 0.15*authorityBoost + 0.15*confidenceBoost` — a linear boost, not a cross-encoder or LLM pass), then allocated against a token budget (`allocateByTokenBudget`). Authorization is namespace-scoped before any search runs (`resolveAuthorisedNamespaces`, `containsAllNamespaces`).

This is real hybrid retrieval — five signal sources, proper RRF fusion — with one clear gap: **the rerank stage has no learned or LLM component**, unlike Hindsight's documented cross-encoder rerank or a Sourcegraph-class embeddings-plus-cross-encoder pipeline.

### 2.7 Consolidation worker — Verified structure, not fully read (`workers/consolidation/src/scos_consolidation/`, 4,024 LOC across the whole `workers/` tree)

Module breakdown confirms a real multi-stage pipeline distinct from the synchronous per-turn logic in memory-intelligence: `extractor.py`, `dedup.py`, `review.py`, `conflicts.py`, `promoter.py`, `synthesis.py`, `llm_enrichment.py`, `models.py`, each with its own test file. Migration `000020_dream_merge_synthesis.sql` exists, naming this pipeline's persistence layer "dream merge synthesis" — SCOS independently uses "dream" terminology for its own background consolidation, same word Honcho uses for its `dreamer` module (again, convergent naming, not shared code — Honcho's dreamer is agentic/LLM-specialist-driven per its source; whether SCOS's is comparably sophisticated was not verified line-by-line in this pass — **Unknown** pending a deeper read of `synthesis.py`/`llm_enrichment.py`).

### 2.8 Context-optimisation — Verified (`services/context-optimisation/cmd/server/main.go`, 1,042 LOC)

`handleInputOptimise`: segments input, allocates a token budget per content class (`allocateBudget`, default 8,000 tokens if unspecified), compresses per a named `compression_profile` with a versioned+hashed `CompressionPolicyVersion`/`CompressionPolicyHash` (so a stored compressed artifact can be traced back to exactly which policy produced it), and exposes `handleContextEvaluate`/`handleContextRestore` to score and reconstruct compressed context. Versioned compression policy is a genuine provenance feature most competitors' context-trimming code does not have.

### 2.9 MCP / agent-integration surface — Verified

Two layers: (1) `adapters/mcp` exposes exactly **one** generic MCP tool, `scos.route`, routing everything through the Autonomous Control Plane; (2) `integrations/claude/plugins/scos-memory-fabric/` is a full Claude Code plugin with hooks (`scos-auto-persist.py`, `scos-auto-recall.py`, `scos-tool-guidance.py` — agents don't have to remember to call memory, matching the README's stated design goal), slash commands (`scos-recall`, `scos-status`, `scos-explain`), and a named MCP tool surface: `scos_recall`, `scos_status`, `scos_correct`, `scos_explain`, `scos_forget`, `scos_code_impact`, `scos_code_index`, `scos_code_query`, `scos_code_repo_get/list/register`, `scos_graphify_explain/path/query`, `scos_memory_graph`, `scos_user_model`. Six more agent runtimes are wired under `integrations/`: `codex`, `hermes`, `kilo`, `openclaw`, `opencode`, `qwen`.

### 2.10 Governance, security, benchmarking — Verified

- `GOVERNANCE.md`: 113,541 bytes, 43 numbered top-level sections (executive ruling through "governing prohibitions" and a "final architecture decision"), RFC-2119 normative language. `make audit` runs `scripts/audit_governance.py` against the *running* system, not just static text — a real CI gate, not documentation.
- 11 ADRs (`docs/adr/0001`–`0011`), each with Context/Decision/Consequences/Compatibility/Migration sections. ADR-0010 (`superiority-benchmark-methodology`, dated 2026-07-29, verified) mandates frozen benchmark corpora, `packages/benchmark-harness`'s `ComputeRecallAtK`/`ComputePrecisionAtK`/`ComputeMRR`/`ComputeNDCGAtK` as the canonical metric implementations, comparison against an unoptimized baseline + prior production version + strongest lawfully-evaluable competitor, and an explicit ban on cherry-picked demonstrations.
- Failure-injection: 58 cases run against a live Compose stack over HTTP (`make failure-injection`), fails closed if the stack isn't running (README, corroborated by `tests/failure-injection/runner.py` existing).
- Zero-secret-default deployment (README claim, consistent with `.env.example` requiring explicit `openssl rand` secrets — not independently re-verified line-by-line in this pass).
- Security scanning in CI: Trivy (container images), gitleaks (secrets), CycloneDX SBOM (documented in README; `.github/workflows/` exists but was not opened in this pass — **Documented**, not Verified).

---

## 3. Competitive Landscape — What Was Actually Inspected

| Project | Repo | HEAD inspected (today, 2026-09-01) | License | Primary language | Depth this pass |
|---|---|---|---|---|---|
| Mem0 | `mem0ai/mem0` | `c33ca27` | Apache-2.0 | Python (+TS client) | Verified — cloned, read `mem0/memory/main.py` in full structure |
| Graphiti (Zep's OSS core) | `getzep/graphiti` | `8b61fce` | Apache-2.0 | Python | Verified — cloned, read `graphiti_core/graphiti.py`, `edges.py` |
| Honcho | `plastic-labs/honcho` | `c300236` | **AGPL-3.0** | Python | Verified — cloned, read `src/deriver/`, `src/dreamer/`, `src/routers/peers.py` |
| Cognee | `topoteretes/cognee` | `e8dd3d9` | Apache-2.0 | Python | Verified — cloned, read pipeline/ontology/retriever module structure |
| Letta / Letta-Code | `letta-ai/letta` (archived) → `letta-ai/letta-code` | `4511fa0` / current | Apache-2.0 | TypeScript (Letta-Code); Python (archived V1) | Documented — cloned both, V1 source is archived-branch-only (not pulled), Letta-Code read via README only |
| Hindsight | `vectorize-io/hindsight` | `ebcd88f` | MIT | Python (+ many client languages) | Documented — cloned, read README's architecture claims only, did not open `hindsight-api` source |
| Sourcegraph SCIP | `sourcegraph/scip` | `f528930` | Apache-2.0 | Protobuf spec + Go/TS bindings | Verified — cloned, read `scip.proto` schema directly |
| Aider (repo-map technique) | `Aider-AI/aider` | `5dc9490` (2026-05-22 — ~3 months stale vs. the others) | Apache-2.0 | Python | Verified — cloned, read `aider/repomap.py` in full |
| Zep (hosted product layer, distinct from Graphiti) | — | — | Proprietary/hosted | — | **Unknown** — not independently inspected; Graphiti is Zep's published OSS core, the hosted product's session/user API layer was not inspected |
| LangMem | `langchain-ai/langmem` | — | — | — | **Unknown** — not cloned in this pass |
| OpenMemory | — | — | — | — | **Unknown** — not cloned in this pass; not to be confused with Mem0's "OpenMemory MCP" sub-feature, which was also not independently verified |
| Joern (code property graphs) | `joernio/joern` | — | — | — | **Unknown** — not cloned in this pass; referenced only from general knowledge, flagged Unverified everywhere it appears below |

Selection logic: I prioritized breadth-with-real-verification over a longer list of names I'd only be quoting a README for. Every project above with a "Verified" or "Documented" tag was cloned to its actual current HEAD today — these are not stale, pre-cutoff descriptions.

---

## 4. Competitor-by-Competitor Deep Dives

### 4.1 Mem0 — Verified

`mem0/memory/main.py` (3,868 lines) implements `Memory`/`AsyncMemory`. The core add-path (`_add_to_vector_store`) is a **"V3 phased batch pipeline"**, verified directly:

1. Phase 0 — pull the last 10 messages of session context (`db.get_last_messages`).
2. Phase 1 — vector-search the top 10 *existing* memories against the new message text, before deciding anything.
3. Phase 2 — **one single LLM call** (`ADDITIVE_EXTRACTION_PROMPT`, optionally + `AGENT_CONTEXT_SUFFIX` when agent-scoped) receives both the new message and the existing-memory candidates (remapped to small integer indices, not real UUIDs — an explicit anti-hallucination measure so the model can't invent an ID) and returns extracted memory items with ADD/UPDATE/DELETE/NONE-style decisions in one shot.
4. Phase 3 — batch-embed everything extracted.

This is architecturally the opposite tradeoff from SCOS: Mem0 makes exactly one LLM call that both extracts *and* decides update/merge in context; SCOS's memory-intelligence path is deterministic-first (heuristic overlap, heuristic contradiction) and only escalates to an LLM above a shortlist threshold. Mem0's approach is more semantically capable per write (the model sees the actual candidate text, not a similarity score) but pays an LLM call on every add where SCOS often doesn't.

Also verified: an **entity store** (`_upsert_entity`/`_link_entities_for_memory`/`_existing_entities_by_text`) that links memories to normalized entity records and boosts search results that hit a matched entity (`_compute_entity_boosts`); **procedural memory** creation as a distinct code path (`_create_procedural_memory`); **expiration** (`_normalize_expiration_date`/`_payload_is_expired` — real TTL, payload-level, checked at read time); an **advanced metadata filter DSL** (`_process_metadata_filters`, `_has_advanced_operators` — comparison operators beyond equality); and a fact-level `history()` method with full mutation log. I could not locate a `graph_memory.py`/graph-store module at the path this class of documentation usually implies in this version of the tree — **Unknown** whether Mem0's graph-memory feature has moved, been renamed, or requires an optional extra not present in the base clone; not confirmed removed, just not found at the expected path in this pass.

### 4.2 Graphiti (Zep's OSS temporal graph core) — Verified

`graphiti_core/graphiti.py`'s `Graphiti` class: `add_episode`/`add_episode_bulk` ingest raw text/message "episodes"; `_extract_and_resolve_nodes`/`_extract_and_resolve_edges` do LLM-driven entity/relationship extraction with resolution against existing graph state; `_extract_and_dedupe_nodes_bulk` batches the same for bulk ingestion; `build_communities` runs graph community detection; `add_triplet` allows manually injecting a fact as a subject-predicate-object triple without going through extraction at all; a **"saga"** concept (`_get_or_create_saga`, `summarize_saga`) groups episodes into a higher-level narrative unit with its own summary — a session/thread-level abstraction above the raw episode.

`edges.py` confirms genuine **bi-temporal** modeling: every edge carries `created_at` (when the row was written), `expired_at` (when it stopped being current, if superseded), `valid_at`/`invalid_at` (when the *fact itself* became true/false in the world) — four independent timestamps, not the two SCOS's `Fact` carries (`observed_at`/`recorded_at` plus `valid_from`/`valid_until`, which conflates "record time" with "fact validity" less cleanly than Graphiti's explicit created/expired-vs-valid/invalid split). This is the single clearest "they have it, we have a partial version of it" finding in the whole audit: **SCOS has temporal fields but not a true bitemporal model** — there is no distinct "when did the system learn this was no longer true" timestamp separate from "when did the underlying fact stop being true."

### 4.3 Honcho — Verified. Also: AGPL-3.0.

Honcho's central abstraction is the **peer**, not "user" and "agent" as asymmetric roles (`src/routers/peers.py`: `get_or_create_peer`, `peer_card`, `peer_context`, `search_peer`, `sessions_for_peer`) — any conversational participant, human or agent, is a peer with symmetric capabilities, including a **peer card** (a theory-of-mind-style standing representation of what the system believes about that peer). Background processing is split into two distinct verbs: `deriver` (`src/deriver/deriver.py`: `process_representation_tasks_batch` — turns raw conversation into structured "representation" facts, the equivalent of SCOS's synchronous fact-extraction but run as an async batch worker instead of inline per-turn) and **`dreamer`** (`src/dreamer/`: `dream_scheduler.py`'s singleton `DreamScheduler` with `schedule_dream`/`execute_dream`/`cancel_dream`; `orchestrator.py`'s `run_dream`/`process_dream`/`run_card_refresh_dream`; and, most notably, `_create_queries_from_surprisal` — Honcho picks *what to reflect on* using an information-theoretic surprisal signal, not a fixed schedule or simple recency). `specialists.py` defines `BaseSpecialist`, an abstract base for **agentic sub-models with their own tools, prompts, and iteration budgets** that a dream can dispatch to — i.e., consolidation itself is done by small dedicated agents, not one monolithic summarization call.

This is architecturally the most sophisticated consolidation design of everything inspected: async derivation, surprisal-driven reflection scheduling, and specialist sub-agents doing the actual reflection work. SCOS's closest equivalent (`handleReflect` + the `workers/consolidation` pipeline) is synchronous-on-request for reflection and does not use a surprisal-style signal to decide what's worth consolidating — it's Unknown (not verified in this pass) whether `workers/consolidation`'s `synthesis.py` has anything comparable.

**License note, material for any "adopt" recommendation below:** Honcho is AGPL-3.0. Directly reusing or adapting Honcho source into SCOS (Apache-2.0-flavored README, MIT-licensed) would trigger AGPL's network-copyleft obligations. The *ideas* (surprisal-driven scheduling, specialist sub-agents, peer symmetry) are freely reimplementable; the code is not freely reusable.

### 4.4 Cognee — Verified

Architecture is explicitly **pipeline/DAG-based**: `cognee/modules/pipelines/tasks/task.py`'s `Task` objects are composed and run via `run_pipeline`, with a pluggable execution mode (`get_pipeline_executor`) — this is the most "data-engineering-native" design of the group; ingestion, extraction, and graph-writing are composable steps a user can reorder or replace rather than a fixed internal sequence. `cognee/modules/ontology/` (`ontology_config.py`, `matching_strategies.py`) supports **ontology-guided extraction** — matching extracted entities against a supplied reference ontology/vocabulary rather than free-form LLM extraction alone; none of the other memory-focused competitors inspected in this pass do this. Retrieval is genuinely pluggable across **multiple graph-completion strategies**, confirmed as separate modules: `graph_completion_retriever.py` (baseline), `graph_completion_cot_retriever.py` (chain-of-thought), `graph_completion_decomposition_retriever.py` (query decomposition), `graph_completion_context_extension_retriever.py`. Graph storage itself is adapter-pluggable, including an embedded option (`cognee/infrastructure/databases/graph/kuzu/` — Kuzu, an embedded graph database, alongside what other adapters imply is also Neo4j support). `memify` tasks include `cognify_agent_trace_feedback.py` — a pipeline specifically for learning from an **agent's own execution traces**, not just conversation, which is close to what a coding-agent-focused memory system (SCOS's actual audience) should want.

### 4.5 Letta / Letta-Code — Documented (README only), with one important, current, dated fact verified

**Verified by cloning `letta-ai/letta` today:** the classic "Letta V1" architecture — the MemGPT-lineage Python server with self-editing core memory blocks and archival memory that most training-data-era descriptions of Letta describe — **has been retired**. The `letta-ai/letta` repo's `main` branch is now a landing page; the README states plainly: *"The current source code lives in `letta-ai/letta-code`... The retired Letta V1 server source is preserved on the `archive` branch."* This is a real, current (as of today), and easy-to-get-wrong fact: anyone benchmarking "Letta" against SCOS using pre-2026 knowledge of Letta's server architecture is benchmarking a retired product.

**Current Letta ("Letta Code"), from its README (Documented, not source-verified in this pass):** a TypeScript coding-agent harness (`npm install -g @letta-ai/letta-code`) whose differentiator is that agents **rewrite their own memory, skills, prompts, and even the harness itself** ("mods"). Two features are directly comparable to SCOS design choices: **MemFS** — all agent context, including memory blocks, is tracked via git and can sync to a custom GitHub repository (`/memory-repository set git@github.com:...`) — a close conceptual parallel to SCOS's OKF git store, but MemFS tracks an agent's own live, self-edited context rather than a separately curated, promotion-gated knowledge tier; and `/palace` as a memory-view command, independently converging on the same "memory palace" naming SCOS's `HierarchyNode` levels use. Also documented: periodic `/sleeptime` dreaming, a `/doctor` memory-quality audit command, subagents (general-purpose/forked/recall/history-analyzer), and Letta Cloud for cross-machine agent-state continuity.

### 4.6 Hindsight — Documented (README only, not source-verified in this pass)

The most productized/DX-forward competitor of the group, and the only one that publishes a live, continuously-updated public benchmark site (`benchmarks.hindsight.vectorize.io`) with a specific claim of #1 on LongMemEval, independently reproduced by an outside research group (Virginia Tech's Sanghani Center) per the README — this is a vendor's own claim about third-party reproduction, not something I independently verified, but it is a materially stronger public evidence posture than any other competitor here, SCOS included (SCOS has no public benchmark result at all yet, despite having the strictest internal benchmark-governance ADR of the group).

API surface is three verbs — **retain / recall / reflect** — mapping closely to SCOS's write/retrieve/reflect split, but with more structure at each stage:

- `retain`: one LLM pass extracts facts, temporal data, entities, and relationships, normalized into canonical form.
- `recall`: **four retrieval strategies run in parallel** — semantic (vector), keyword (BM25), graph (entity/temporal/causal links), temporal (time-range filter) — fused via RRF, then **passed through a cross-encoder reranking model**, then trimmed to a token limit. This is one stage more sophisticated than SCOS's retrieval-orchestrator: SCOS has the same four-ish signal sources (exact/FTS/vector/relation, plus session state) and RRF fusion, but its post-fusion rerank is a linear deterministic boost, not a trained cross-encoder.
- `reflect`: deeper, slower analysis for questions needing synthesis rather than lookup.

Distinctive concepts with no SCOS equivalent found in this pass: **observations** (consolidated, evidence-backed beliefs, kept with exact supporting quotes and a proof count, *refined* rather than overwritten as new evidence arrives — SCOS's belief/supersession model overwrites/supersedes rather than incrementally strengthening a standing belief); **mental models** (a standing answer to a fixed question, written once, silently rewritten in the background as the bank learns more, and read at zero marginal cost — a plain database read, no retrieval pass, no LLM call — versus SCOS paying full retrieval on every read); **knowledge pages** (mental models projected onto disk as a browsable, syncable markdown wiki — closer to what OKF is for, but generated/maintained automatically rather than promotion-gated); **memory banks** with per-bank **disposition traits** (skepticism/literalism/empathy) that shape how `reflect` reasons; **multilingual-by-default** entity preservation (native script kept, not transliterated); and **Memory Defense**, an opt-in per-bank secret/PII scanner (45 patterns) that redacts or blocks at `retain` time — SCOS has no equivalent write-time content-safety scan located in this pass (Unknown whether one exists in `security-gate` — plausible given the service name, not verified).

### 4.7 Sourcegraph SCIP — Verified (protocol spec only, not a running Sourcegraph instance)

`scip.proto`, read directly: `Index` → `Document`s → `Symbol`/`SymbolInformation` (with `Relationship`s: implements/extends/references-to-supertype etc.) and `Occurrence`s (position-tagged references with `SymbolRole` — definition vs. reference vs. write-access etc.) and `Diagnostic`s. This is a compact, language-agnostic, compiler-or-language-server-derived precision protocol — the intended successor to LSIF. SCOS already consumes this format as its Tier-1-precision code-intelligence ingest path (`scipimport.go`, §2.4) — SCOS is a SCIP *consumer*, not a competitor to SCIP itself; the comparison that matters is SCOS-as-a-platform vs. Sourcegraph-as-a-platform (search UI, cross-repo navigation, code intel at IDE-hover latency), which was **not inspected** in this pass (Sourcegraph's actual server is not fully open-source in the same way; Unknown beyond the protocol).

### 4.8 Aider's `repomap.py` — Verified, and the single most cost-effective technique found in this entire audit

867 lines, no LLM call, no embeddings, no external service. `RepoMap.get_tags`/`get_tags_raw` extracts definitions and references per file using cached Tree-sitter tag queries (a `ctags`-equivalent tagging scheme); `get_ranked_tags` builds a `networkx` directed graph of file-to-file reference relationships and runs **`networkx.pagerank`**, optionally personalized toward files the user has mentioned in chat or edited recently (`pers_args`); `render_tree`/`to_tree` then render a token-budgeted map of the highest-ranked symbol signatures per file. The entire "which code should the model see right now" decision is graph centrality plus recency personalization — cheap, deterministic, explainable, and does not require an LLM round-trip or an embedding index to compute. This is a materially different, and materially cheaper, answer to "what code context matters for this task" than anything in SCOS's `context-optimisation` or `code-intelligence` services, which currently allocate token budget by content class and compression profile, not by graph centrality of the code itself. (Aider's last commit in this clone is 2026-05-22 — roughly three months stale relative to the other repos audited today; treat its current maintenance velocity as **Unknown**, not assumed-healthy, even though the technique itself is sound and inspectable regardless of the project's current cadence.)

---

## 5. Detailed Competitive Feature Matrix

Status values: **Strong** (real, verified, production-shaped) · **Partial** (real but limited/shallow) · **Basic** (present but minimal) · **Missing** · **Unknown** (not verified this pass).

| Capability | SCOS | Mem0 | Graphiti/Zep | Honcho | Cognee | Letta-Code | Hindsight | Best implementation |
|---|---|---|---|---|---|---|---|---|
| Fact extraction from conversation | Strong (deterministic + LLM-gated) | Strong (single-call V3 pipeline) | Strong (episode extraction) | Strong (async deriver) | Strong (pipeline-based) | Unknown | Strong (retain) | Mem0 (fewest LLM calls per useful fact) |
| Explicit supersession lineage | Strong (`supersedes_id`) | Strong (update/delete decisions) | Strong (bitemporal expiry) | Unknown | Unknown | Unknown | Strong (observations refine) | Graphiti (true bitemporal) |
| True bitemporal (valid vs. recorded time, both directions) | Partial (has both but not fully independent) | Unknown | **Strong** | Unknown | Unknown | Unknown | Unknown | Graphiti |
| Contradiction detection | Basic (negation-word heuristic) | Unknown | Unknown | Unknown | Unknown | Unknown | Unknown | None of the inspected set confirmed strong here — general gap |
| Duplicate/merge detection | Partial (heuristic shortlist + LLM confirm) | Strong (LLM sees candidates directly) | Strong (extract-and-resolve) | Unknown | Unknown | Unknown | Strong (proof-count observations) | Hindsight (incremental strengthening, not just merge) |
| Standing/precomputed answers (zero-latency read) | **Missing** | Missing | Missing | Partial (peer card is close) | Missing | Missing | **Strong (mental models)** | Hindsight |
| Consolidation scheduling signal | Basic (on-demand `/reflect` call) | Unknown | Unknown | **Strong (surprisal-driven)** | Unknown | Documented (`/sleeptime`) | Documented (background) | Honcho |
| Entity store + entity-boosted retrieval | Missing (entities exist, no boost verified) | **Strong** | Strong (graph nodes ARE entities) | Strong (peer card) | Strong (ontology-matched) | Unknown | Strong | Mem0/Graphiti tie |
| Reviewed/curated knowledge tier separate from raw memory | **Strong (OKF)** | Missing | Missing | Missing | Missing | Partial (MemFS, self-edited not reviewed) | Partial (knowledge pages, auto-written not reviewed) | SCOS |
| Multi-tenant isolation at DB layer (RLS) | **Partial — policy well-designed, not enforced against the connecting role (corrected)** | Missing (app-layer filter) | Missing | Missing (app-layer) | Missing | N/A (single-user harness) | Unknown | SCOS has the best foundation, none has a working DB-layer enforcement verified in this pass |
| Local-first embeddings by default | **Strong (bge-small, self-hosted)** | Missing (defaults to hosted API) | Unknown | Unknown | Partial (pluggable) | N/A | Partial (25+ providers incl. local) | SCOS/Hindsight tie |
| Hybrid retrieval fusion | Strong (RRF, 5 sources) | Partial (vector-first) | Partial (graph-first) | Unknown | Strong (pluggable retrievers) | Unknown | **Strong (4-way RRF)** | Hindsight/SCOS tie |
| Learned/cross-encoder rerank | **Missing (linear boost only)** | Unknown | Unknown | Unknown | Unknown | Unknown | **Strong (documented)** | Hindsight |
| Code symbol extraction precision | Strong (3-tier: AST→Tree-sitter→SCIP) | N/A | N/A | N/A | N/A | N/A | N/A | SCOS (only code-native system here) |
| Code call-graph / change-impact analysis | Strong (BFS over CALLS edges) | N/A | N/A | N/A | N/A | N/A | N/A | SCOS |
| Dense/semantic code search | **Missing (deferred)** | N/A | N/A | N/A | N/A | N/A | N/A | Sourcegraph-class systems (not independently verified) |
| Graph-centrality-based context selection | Missing | N/A | N/A | N/A | N/A | N/A | N/A | **Aider (PageRank repo-map)** |
| Rename-lineage-aware symbol identity | **Strong** | N/A | N/A | N/A | N/A | N/A | N/A | SCOS |
| Immutable snapshot/generation model | **Strong** | N/A | N/A | N/A | N/A | N/A | N/A | SCOS |
| PII/secret scanning at write time | Unknown (plausibly `security-gate`, not verified) | Missing | Missing | Missing | Missing | Unknown | **Strong (Memory Defense, 45 patterns)** | Hindsight |
| Named framework/agent integrations | Partial (7 runtimes, 1 plugin) | Partial | Partial | Partial | Partial | Strong (channels: Slack/Telegram/Discord/mobile) | **Strong (50+)** | Hindsight |
| Published, reproducible public benchmark | **Missing** | Unknown | Unknown | Unknown | Unknown | Unknown | **Strong (live site, 3rd-party reproduced per README)** | Hindsight |
| Internal benchmark-methodology governance (frozen corpora, anti-cherry-pick) | **Strong (ADR-0010)** | Unknown | Unknown | Unknown | Unknown | Unknown | Unknown | SCOS (only one confirmed to require this of itself) |
| Ontology/schema-guided extraction | Missing | Missing | Missing | Missing | **Strong** | Unknown | Unknown | Cognee |
| Pluggable/composable pipeline architecture | Partial (fixed Go service pipeline) | Partial | Partial | Partial | **Strong (Task/DAG)** | Unknown | Unknown | Cognee |
| Copyleft-clean license for adoption | Strong (MIT) | Strong (Apache-2.0) | Strong (Apache-2.0) | **AGPL-3.0 — caution** | Strong (Apache-2.0) | Strong (Apache-2.0) | Strong (MIT) | — |

---

## 6. What They Have That We Don't (ranked by value, not by volume)

Each entry: what it is, where it's real, why it matters, difficulty, and a concrete recommendation.

### 6.1 True bitemporal fact model — Graphiti — HIGH value, MEDIUM difficulty
**Evidence:** `graphiti_core/edges.py` — `created_at`, `expired_at`, `valid_at`, `invalid_at`, four independent fields (Verified).
**Why it matters:** lets a system answer "what did we believe was true, as of date X" *and* "what was actually true as of date X" as two different questions. SCOS's `Fact` conflates these into `observed_at`/`recorded_at`/`valid_from`/`valid_until` without a clean invalidation-time field distinct from validity-end.
**Recommendation:** add `belief_expired_at` (system-time: when SCOS itself stopped trusting this fact, e.g. via supersession) as distinct from `valid_until` (world-time: when the fact stopped being true in reality). This is a schema migration plus a `createOrSupersedeFact` change, not a new service.
**Priority:** P1.

### 6.2 Standing precomputed answers ("mental models") — Hindsight — HIGH value, MEDIUM difficulty
**Evidence:** Hindsight README (Documented, not source-verified).
**Why it matters:** every SCOS read pays full retrieval-orchestrator cost (5-source fan-out + RRF). A small set of high-frequency questions per namespace ("what does this user prefer", "what's this project's current architecture") could be answered from a single row, rewritten in the background whenever new evidence lands, instead of recomputed on every ask.
**Recommendation:** a new `memory_standing_answers` table keyed by `(namespace_id, question_key)`, refreshed by a background worker (extend `workers/index-maintenance` or add a small one) whenever relevant facts change; `retrieval-orchestrator` checks this table first and only falls through to full retrieval on a miss.
**Priority:** P1.

### 6.3 Surprisal-driven consolidation scheduling — Honcho — MEDIUM value, HIGH difficulty
**Evidence:** `src/dreamer/orchestrator.py:_create_queries_from_surprisal` (Verified).
**Why it matters:** deciding *what's worth reflecting on* by an information-theoretic surprise signal (how much a new observation changes the model's existing belief) is more principled than a fixed schedule or manual `/reflect` call, and avoids wasting consolidation compute on redundant, expected information.
**Recommendation:** this is a genuine research technique, not a quick port (Honcho is AGPL-3.0 besides — reimplement the idea, don't copy the code). Track as a differentiator opportunity for `workers/consolidation`, not a near-term item.
**Priority:** P2/moat candidate — see §25.

### 6.4 Cross-encoder rerank stage — Hindsight (documented), general best practice — MEDIUM value, LOW-MEDIUM difficulty
**Evidence:** Hindsight README states cross-encoder rerank after RRF (Documented).
**Why it matters:** SCOS's current rerank (`0.15*authority + 0.15*confidence` linear boost) doesn't look at query-result semantic relevance at all post-fusion — it only reorders by metadata. A cross-encoder (even a small local one, matching SCOS's local-embeddings-by-default philosophy) would catch cases where RRF's rank agreement across sources still leaves semantically-weak results near the top.
**Recommendation:** add an optional cross-encoder rerank stage to `retrieval-orchestrator` (e.g., a local `cross-encoder/ms-marco-MiniLM` class model served by a small addition to `workers/embeddings` or a new lightweight worker), gated the same way vector search already gates on embedding-service availability (explicit fallback to the existing linear rerank, not silent degrade).
**Priority:** P1.

### 6.5 Ontology/schema-guided extraction — Cognee — MEDIUM value, MEDIUM difficulty
**Evidence:** `cognee/modules/ontology/` (Verified structure).
**Why it matters:** free-form LLM extraction drifts entity/relation naming over time ("uses Postgres" vs "backed by PostgreSQL" as two different facts). A supplied reference ontology constrains extraction to canonical entity/relation types.
**Recommendation:** worth prototyping against SCOS's `entity` extraction path specifically (a fixed vocabulary of entity *types* — service, host, decision, person — is a small, high-leverage addition given SCOS already has typed `MemoryNetwork`s to hang it off of).
**Priority:** P2.

### 6.6 PII/secret scanning at write time — Hindsight's "Memory Defense" — MEDIUM value, LOW difficulty
**Evidence:** Hindsight README (Documented, 45-pattern claim not independently verified).
**Why it matters:** a memory system that stores a leaked API key or SSN because a user pasted it into a conversation is a real, boring, high-frequency failure mode.
**Recommendation:** if `security-gate` doesn't already do this (Unknown — check before building), add a pattern-scan step on the write path (secret-scanning patterns already exist in this repo's own CI tooling, per the README's mention of gitleaks — the same pattern library can plausibly be reused/adapted rather than hand-rolled).
**Priority:** P0 if not already present — this is a safety gap, not a feature gap, if confirmed missing.

### 6.7 Graph-centrality-based context selection — Aider — HIGH value for the code-intelligence plane specifically, LOW difficulty
**Evidence:** `aider/repomap.py` (Verified in full).
**Why it matters:** cheapest, most explainable technique in this entire audit. No LLM call, no embedding index — a PageRank pass over the existing `code_edges` graph SCOS already has (from `goextract.go`/`treesitter.go`/`scipimport.go`) would immediately give `context-optimisation` and `code-intelligence` a principled "which symbols matter most for this task" ranking, personalizable toward recently-changed or explicitly-mentioned files/symbols exactly the way Aider does it.
**Recommendation:** SCOS already has the graph (`code_edges`); this is a retrieval-ranking addition, not new extraction infrastructure. Genuinely the highest-value/lowest-cost item in this entire report.
**Priority:** P0/P1 — see the backlog.

### 6.8 Massive integration surface — Hindsight — LOW technical value, HIGH go-to-market value
**Evidence:** 50+ named directories under `hindsight-integrations/` (Verified — directory listing only, contents not read).
**Why it matters:** this is mostly marketing surface area, not depth — but it lowers adoption friction dramatically, and "works with your existing Claude Pro/ChatGPT Plus subscription, no API key" is a genuinely clever go-to-market move that removes a real cost objection.
**Recommendation:** not a priority engineering item; worth noting for whoever owns SCOS's adoption strategy, not its architecture.
**Priority:** P3 / product, not engineering.

---

## 7. What We Have That They Don't (real differentiators, not invented ones)

1. **A better-designed RLS policy layer than any competitor's application-side filtering — not yet an enforced advantage (corrected).** (§2.2, Verified, correction applied same-day.) Every other memory-focused competitor inspected in this pass isolates by application-layer filter (a `WHERE user_id = ?` a future contributor can forget to add). SCOS's policies are namespace-membership-scoped and well-designed — genuinely better *foundation* than a scattered app-layer filter. But as deployed today, they're not enforced against the connecting role (no `FORCE ROW LEVEL SECURITY`; services connect as the table owner, which Postgres exempts), per this repo's own `audit-results/` hostile-security-audit, independently verified against current HEAD. **This is now the top of the P0 list, not a strength to lead with** — see §21/§24's added P0 item. **Also to check once fixed:** whether RLS coverage extends to the newer `code_*` tables from the code-intelligence rewrite (Unknown in this pass — the RLS policy files inspected predate `000025`–`000029`).
2. **A separate, promotion-gated, git-versioned curated-knowledge tier (OKF) distinct from the raw record** (§2.3, Verified). Letta-Code's MemFS and Hindsight's knowledge pages are the closest analogues, and neither has SCOS's explicit LLM-classify-then-human-reviewable-proposal gate — both are self-written/auto-rewritten, not promotion-reviewed. **To strengthen:** the promotion path currently only fires from `memory-intelligence`'s auto-promotion check; extend it to `code-intelligence` (an architecture decision discovered via change-impact analysis is exactly OKF-shaped knowledge) and `workers/consolidation` (a synthesized cross-session insight is too) — right now OKF's only writer is one service.
3. **Immutable snapshot/generation model with rename-lineage-aware symbol identity** (§2.4, Verified). None of the memory-focused competitors have a code plane to compare; this is a real code-intelligence architecture strength on its own terms, not just "ahead of the old audit's assessment."
4. **A governance ADR that requires benchmark rigor before any superiority claim, enforced as process** (§2.10, Verified). No competitor's public repo documents an equivalent internal discipline — though note the asymmetry: SCOS has the *process* and no published *result* yet; Hindsight has a published result and (from outside) an unknown internal process. A rigorous process nobody outside the team can see is not yet a competitive advantage in the market — it's a precondition for building a credible one. **The gap this creates: run the first ADR-0010-compliant benchmark and publish it.**
5. **Local-first embeddings by default** (§2.7, Verified: `BAAI/bge-small-en-v1.5`, self-hosted). Consistent with a genuinely local-first design philosophy; Mem0 defaults to a hosted API. Worth stating explicitly in any comparison material, since it's a real cost and privacy differentiator, not a technicality.
6. **A single unified evidence model spanning both memory and code** (Inferred, not fully realized): SCOS's `Fact.Evidence`, `TemporalRelation`, and `code_edges` share the same underlying philosophy (typed, evidenced, provenance-tracked) even though they're not yet cross-linked (§9, §25) — no competitor inspected in this pass has both a memory plane and a code-intelligence plane at all, let alone one designed to eventually connect them. This is architecture *positioning* for a moat (§25), not yet a shipped feature — do not oversell it as more than that today.

---

## 8. Persistent Memory Analysis — Synthesis

Across SCOS + 6 memory-focused competitors, the lifecycle stages that are **solved well by at least one system, verified**: extraction (Mem0's single-call phased pipeline is the most LLM-call-efficient; Honcho's async deriver is the most decoupled from request latency), supersession/lineage (SCOS and Mem0 both have explicit lineage), bitemporal validity (Graphiti, clearly ahead), consolidation scheduling (Honcho's surprisal signal is the most principled), standing-answer caching (Hindsight, clearly ahead), and multi-tenant isolation (SCOS, clearly ahead). The stage that is **weak or unverified everywhere**, SCOS included: genuinely semantic contradiction detection. Every negation-word or bag-of-words heuristic (SCOS's `isContradictory`) will miss same-polarity factual conflicts ("ships on Tuesdays" vs "ships on Thursdays") and false-positive on unrelated negated sentences sharing a long word. None of the six competitors' READMEs or inspected source in this pass claimed a semantic (embedding-distance-plus-NLI-style) contradiction detector either — this is a general gap in the field, not just a SCOS weakness, and is worth flagging as a genuine research opportunity rather than "catch up to competitor X," because no competitor X inspected here has solved it.

---

## 9. Code Indexing Analysis — Synthesis

SCOS is the only system in the comparison set with a code-intelligence plane at all (Mem0/Graphiti/Honcho/Cognee/Letta-Code/Hindsight are all conversation/agent-memory systems with no code-symbol-graph equivalent verified in this pass). The honest comparison is therefore SCOS vs. the state of the art in dedicated code-intelligence tooling, not SCOS vs. its memory-system peer group:

- **Precision ladder** (Go-native AST → Tree-sitter → SCIP, §2.4) is a sound design: cheap syntactic coverage everywhere, exact semantic precision where a caller supplies a real indexer's output. This mirrors how Sourcegraph itself layers Tree-sitter-based "search-based" navigation under SCIP-based "precise" navigation — SCOS independently arrived at the same layering.
- **What's missing versus a Sourcegraph-class system:** dense/semantic code search (deferred, per SCOS's own migration comment), cross-repository graph (each `code_repositories` row is independent; no verified cross-repo edge type), and a resolved (not just syntactic-name) call graph for non-Go languages — Tree-sitter's `treeSitterCalledNames` is name-only, same ambiguity-drop behavior as the Go path but without even Go's type-adjacent AST precision.
- **What's missing versus Aider's much smaller repo-map:** graph-centrality-based ranking for context selection (§6.7) — SCOS has the graph, not yet the PageRank pass over it.

---

## 10. Code Graph Analysis — Synthesis

Current SCOS code graph (Verified, `database/migrations/000025`+): nodes are `code_repositories` → `code_repository_snapshots` → `code_files`/`code_file_versions` → `code_symbols`/`code_symbol_versions` → `code_chunks`; the one populated edge type is `CALLS` (from `goextract.go`/`treesitter.go`'s syntactic extraction) plus whatever relationship/reference edges SCIP ingestion adds (`insertSCIPRelationshipEdgesLocked`/`insertSCIPOccurrenceReferenceEdgesLocked` — implements/extends/reference-to-definition, per SCIP's `Relationship`/`Occurrence` messages, Verified in `scipimport.go`).

Against the target node/edge taxonomy in the original request (repositories/branches/commits/files/packages/classes/functions/symbols/types/schemas/tables/API endpoints/tests/docs/config/external services/authors/issues/PRs, with CONTAINS/DEFINES/IMPORTS/CALLS/IMPLEMENTS/EXTENDS/DEPENDS_ON/TESTS/DOCUMENTS/MODIFIED_BY/RENAMED_FROM etc.): SCOS has repositories, snapshots-as-commit-proxies, files, symbols, and CALLS/SCIP-relationship/RENAMED_FROM edges. It does **not yet have**, verified absent in this pass: package/module-level nodes as first-class graph entities (packages exist as a Go-source concept in `goextract.go` but not as a queryable graph node), test-to-code edges, doc-to-code edges, schema/table nodes, API-endpoint nodes, or issue/PR nodes. **Deterministic vs. probabilistic edges:** every edge type currently populated (CALLS, SCIP relationships, RENAMED_FROM) is deterministically derived from parse output — SCOS has built no probabilistic/LLM-inferred graph edges yet at all, which is a clean, defensible position (no confidence-scoring infrastructure needed yet because nothing uncertain has been added), but also means none of the "why did we build it this way" connective tissue (decision→commit, requirement→symbol) exists — that would necessarily be probabilistic/LLM-assisted, and is exactly the gap §6.7/§25's moat discussion addresses.

---

## 11. Retrieval Analysis — Synthesis

Already covered in depth per-system in §4 and the matrix (§5). The one cross-cutting synthesis point: **every hybrid-retrieval system inspected (SCOS, Hindsight, Cognee) converges on RRF as the fusion method** — this is now clearly the field's default, not a differentiator either direction. The differentiator is what happens *after* fusion: Hindsight's cross-encoder, SCOS's linear metadata boost, Cognee's pluggable multi-strategy retrievers (which the caller picks per-query-type rather than a single universal path). SCOS's retrieval-orchestrator would benefit most from (a) the cross-encoder rerank (§6.4) and (b) Cognee's idea of letting the caller pick a retrieval *strategy* rather than always running the same fixed five-source fan-out — a "what changed in the last 6 months" query and a "who calls this method" query have almost nothing in common and shouldn't necessarily run through the same fusion pipeline.

---

## 12. Temporal and Git-Aware Intelligence

SCOS's code-intelligence snapshot model (§2.4) is genuinely commit-aware and branch-aware (unique on `repository_id, branch, commit_id, extractor_version, configuration_hash`) with real diffing (`DiffSnapshot`) — this is Verified and solid. What's not yet built, per this pass: automatic re-indexing triggered by a file watcher or git hook (indexing appears to be caller-invoked via `scos_code_index`/`handlePGIndex`, not event-driven — Inferred from the absence of any watcher/webhook code found in `services/code-intelligence`, not exhaustively confirmed absent); dependency-ripple invalidation (if symbol A's callers changed, is anything downstream of A's *cached retrieval results* invalidated? Unknown — not traced); and priority-queued/resumable incremental indexing for very large repos (the current model indexes a whole snapshot per call, not incrementally per changed file — Inferred from the API shape, `IndexSnapshot` takes a full file list).

---

## 13. Unified Memory + Code Knowledge Architecture

This is the clearest, most defensible moat opportunity in the whole audit, and it is **not yet built** — be precise about that. SCOS has, verified, separately: a `Fact`/`Entity`/`TemporalRelation` graph with typed evidence in memory-intelligence, and a `code_symbols`/`code_edges` graph with typed provenance in code-intelligence. **Nothing in this pass located a cross-link between the two** — no `Fact` row references a `code_symbol_version_id`, no code-intelligence table references a `memory-intelligence` fact or decision. The README's and prior audit's claim of "commit-aware autobiographical intelligence" (a decision pointing to the conversation, task, commits, and symbols it produced) is **architecturally plausible given the two evidence models' shared shape, but not implemented** — this is the single biggest gap between SCOS's stated ambition and its current, verified reality. See §22/§25 for the concrete target schema and moat ranking.

---

## 14. Storage and Infrastructure Analysis

PostgreSQL (+ pgvector) as sole authoritative store, OKF as a parallel git store — this pairing is deliberate and matches the "one canonical evidence model" principle the (superseded) prior blueprint document also argued for; I independently agree with it rather than recommending a database-sprawl architecture (a separate vector DB, a separate graph DB, a separate document store) purely because competitors use one. **Reasoning, not just agreement:** every competitor inspected that *does* use a dedicated graph database (Cognee's Kuzu/Neo4j adapters, Graphiti's Neo4j) pays real operational cost for it (a second database to run, back up, and keep consistent with the primary store) in exchange for native graph-traversal query performance Postgres's recursive CTEs can't match at very large scale. SCOS's `ChangeImpact` BFS (§2.4) is already doing multi-hop graph traversal in Postgres successfully at its current scale; the honest trigger point for reconsidering (see §21) is when `code_edges` or `memory_relations` row counts make Postgres recursive traversal latency unacceptable — not before, and not because a competitor's architecture diagram has a separate graph-DB box.

---

## 15. Performance and Scalability

**Not independently benchmarked in this pass** — no load test was run against either SCOS or any competitor; this section is Unknown/Inferred only, and should be stated as such rather than populated with invented numbers. What is Verified: SCOS's `docs/scaling.md` itself documents the honest current state — the reference deployment is single-host Compose, most services are stateless and RLS-scoped (real scale-out readiness), and "a handful of services with local disk state" are named as the current scale-out blocker (the doc was not read in enough depth in this pass to name which services — recommend that as a direct follow-up, it's a five-minute read). Hindsight publishes latency/cost numbers on its public benchmark site per its README (Documented, not verified). No other competitor inspected in this pass publishes performance numbers in-repo. **Recommendation, not a finding:** SCOS's own ADR-0010 already mandates the right process for closing this gap — the missing piece is simply running it, once, against a real corpus, and publishing the result the way Hindsight does.

---

## 16. Evaluation and Benchmarking

SCOS already has the right internal scaffolding (`packages/benchmark-harness`'s `ComputeRecallAtK`/`ComputePrecisionAtK`/`ComputeMRR`/`ComputeNDCGAtK`, `benchmarks/harness.py`, a frozen-corpus requirement) and the right governance (ADR-0010) — what it lacks, verified by absence: any benchmark result actually run and published against an external system. Concrete recommendation: build a small, honest benchmark suite in the shape ADR-0010 already requires, covering (a) memory: a LongMemEval-style long-horizon recall set (the same public benchmark Hindsight cites, which makes a same-benchmark comparison directly meaningful rather than inventing a new one nobody else has run) and (b) code: a change-impact-accuracy set built from SCOS's own commit history (does `ChangeImpact` correctly predict what a real historical commit actually touched downstream?) — this second one is uniquely available to SCOS because it already has real git history to mine ground truth from, which is a genuine advantage over building a synthetic benchmark from scratch.

---

## 17. Developer Experience

Verified: a real Claude Code plugin with three lifecycle hooks (auto-persist, auto-recall, tool-guidance) plus three slash commands and ~17 named MCP tools (§2.9) — a materially real DX investment, not a stub. Six more agent runtimes wired under `integrations/`. Against Hindsight's 50+ integrations and Letta-Code's multi-channel (Slack/Telegram/Discord/mobile/desktop) reach, this is an order of magnitude smaller surface — but note the difference in kind, not just count: SCOS's hooks make memory *automatic* for the one runtime (Claude Code) it's deepest on, which is arguably higher-value per integration than broad-shallow coverage across 50 frameworks that each only get basic add/search calls. No admin UI / graph visualization was located as a standalone product in this pass beyond the `transparency-projection` service + `dashboard/` browser client (Verified to exist, contents/screenshots not inspected) — Unknown how polished this is relative to, say, a dedicated memory-inspection UI.

---

## 18. Security and Enterprise Readiness

**Corrected same-day:** RLS-on-every-table (§2.2, Verified) does *not* currently put SCOS ahead of competitors on enforced data isolation — the policies are well-designed but inert against the connecting role, per this repo's own `audit-results/` hostile-security-audit (2026-09-02 run, finding AUTH-001; corroborated by the earlier `comparative-divergence-audit.md`'s F-08), independently verified against current HEAD: no `FORCE ROW LEVEL SECURITY` anywhere in `database/`, both deployment paths connect as the table-owning role. As deployed, SCOS's isolation guarantee is currently in the same category as Mem0/Graphiti/Honcho/Cognee's application-layer filtering — arguably worse in one respect, since a defense presented as database-enforced creates false confidence an app-layer filter doesn't. **This is now the highest-severity item in this entire report** — see §21/§24's added P0. Separately, the same audit process reports mcp-gateway's tenant-header-spoofing vulnerability (its F-01) has genuinely been fixed since it was found — verified directly in `resolveIdentity`, §2.9 update. Zero-secret-default deployment remains Documented, not independently traced in this pass. Honcho's AGPL-3.0 license is worth flagging as an enterprise-adoption friction point for anyone choosing Honcho. Whether write-time PII/secret scanning exists (§6.6) remains an open question, not a confirmed gap.

---

## 19. Technologies Competitors Use That We Don't

| Technology | Used by | Purpose | Why it matters | SCOS equivalent | Gap | Adopt / Reimplement / Ignore |
|---|---|---|---|---|---|---|
| `networkx` + PageRank | Aider | Graph-centrality context ranking | Cheap, explainable, no-LLM context selection | None | Real gap | **Adopt** — SCOS already has the graph; this is a ranking pass, not new infra (§6.7) |
| Cross-encoder reranker | Hindsight (documented) | Post-fusion semantic rerank | Catches RRF's blind spot (rank agreement ≠ relevance) | Linear metadata boost only | Real gap | **Adopt** — small local model, consistent with SCOS's local-first embeddings stance |
| Bitemporal edge schema (4 independent timestamps) | Graphiti | Distinguish fact-validity time from belief-recorded time | Correct "what did we believe when" answers | Partial (fields exist, not fully independent) | Real gap | **Reimplement** — schema change, no code to port (proprietary-shaped already) |
| Ontology/reference-vocabulary matching | Cognee | Canonical entity/relation typing | Prevents entity-name drift over time | None | Real gap | **Adopt** the idea; SCOS's typed `MemoryNetwork`s are a natural anchor |
| Kuzu (embedded graph DB) | Cognee | Local-first graph storage without a server | Matches local-first philosophy without Postgres recursive-CTE limits | Postgres recursive CTEs | Partial gap, not urgent | **Ignore for now** — only revisit at the scale trigger in §21, Postgres is working today |
| Tree-sitter tag queries (`.scm` files, ctags-equivalent) | Aider | Fast def/ref extraction across many languages | SCOS's Tree-sitter path already does this at the AST-node-type level; Aider's tag-query approach is more portable across new languages | Already comparable | No real gap | **Ignore** — SCOS's approach is at least as good, verified |
| SCIP protobuf | Sourcegraph (spec), consumed by SCOS already | Precise, language-agnostic symbol identity | Already adopted | Already adopted | No gap | **N/A — already adopted** |

---

## 20. What They Have That We Don't — Exhaustive Inventory

(Consolidating and formalizing §6 into the requested per-item format; items already detailed in §6 are cross-referenced rather than repeated in full.)

1. **True bitemporal fact model** — Graphiti — `graphiti_core/edges.py` — genuinely valuable — Medium difficulty — schema-only — see §6.1.
2. **Standing precomputed answers ("mental models")** — Hindsight — documented, not source-located — genuinely valuable — Medium difficulty — new table + background worker — see §6.2.
3. **Surprisal-driven consolidation scheduling** — Honcho — `src/dreamer/orchestrator.py` — genuinely valuable but research-grade, AGPL-licensed source (reimplement, don't port) — High difficulty — see §6.3, §25.
4. **Cross-encoder rerank** — Hindsight (documented) — genuinely valuable, standard IR practice, not exotic — Low-Medium difficulty — see §6.4.
5. **Ontology-guided extraction** — Cognee — `cognee/modules/ontology/` — valuable, moderate effort — Medium difficulty — see §6.5.
6. **Write-time PII/secret scanning** — Hindsight (documented) — valuable, possibly already partially covered by `security-gate` (Unknown, verify first) — Low difficulty if net-new — see §6.6.
7. **Graph-centrality context ranking (PageRank over the reference graph)** — Aider — `aider/repomap.py` — highest value-per-effort item in this entire report — Low difficulty, SCOS already has the graph — see §6.7.
8. **Symmetric peer model (vs. asymmetric user/agent)** — Honcho — `src/routers/peers.py` — architecturally interesting, not clearly superior for SCOS's actual use case (agent-assisting-one-developer, not multi-human-peer chat) — judgment call, not a clear "adopt": SCOS's user/agent asymmetry matches its actual product shape (governed agents serving a human, not peers of equal standing) more naturally than Honcho's chat-platform-shaped peer model would. **Recommendation: do not adopt** — noted for completeness, not because it's missing value SCOS needs.
9. **Pluggable multi-strategy retrieval (caller picks the retrieval algorithm per query type)** — Cognee — module structure — valuable, moderate effort, ties into §11's synthesis — Medium difficulty.
10. **Agent-execution-trace-specific consolidation** — Cognee's `cognify_agent_trace_feedback.py` — directly relevant to SCOS's actual audience (coding agents), not just conversational memory — worth prioritizing over some of the above precisely because it's closer to what SCOS's users actually do (edit code, not chat casually) — Medium difficulty.
11. **git-tracked, agent-self-editable context (MemFS)** — Letta-Code — documented — conceptually adjacent to OKF but serving a different purpose (live agent scratch-state vs. curated knowledge) — **do not conflate with OKF**; if built, it's a third tier, not a replacement for either existing one — Medium-High difficulty, genuinely new subsystem.
12. **Massive integration/channel surface** — Hindsight/Letta-Code — mostly go-to-market value, not architecture — see §6.8 — Low technical difficulty per integration, high aggregate effort, product decision not engineering priority.

---

## 21. Architectural Problems in scos-memory

1. **Retrieval rerank has no learned component.** Current: linear authority/confidence boost post-RRF (`services/retrieval-orchestrator/cmd/server/main.go:832`, Verified). Why it becomes a problem: as fact volume per namespace grows, RRF's rank-agreement signal alone increasingly surfaces results that agree across sources but aren't actually the most relevant to a specific query's semantics. Severity: Medium today, rising with scale. Trigger point: once a single namespace holds enough facts that top-20 RRF results routinely include 3+ only-tangentially-relevant items (a number that needs an actual benchmark run to pin down, not a guess — see §16). Superior architecture: add a cross-encoder stage (§6.4). Migration path: additive — new optional stage behind the same fallback pattern already used for vector search availability; no schema change. Compatibility: fully backward compatible.

2. **No cross-link between the memory evidence graph and the code evidence graph.** Current: two structurally similar but entirely disconnected graphs (§13, Verified absent). Why it becomes a problem: it's the exact gap between SCOS's stated product ambition ("commit-aware autobiographical intelligence") and what's actually queryable today — an agent cannot currently ask "why does this function exist" and get an answer grounded in the conversation/decision that produced it, despite both halves of that answer already living in the database. Severity: High, because it's the core differentiator, unbuilt. Trigger point: now — this should be treated as a live gap, not a someday item. Superior architecture: a `memory_code_links` join table (§22). Migration path: additive schema, plus wiring at the two write points where a link is knowable (OKF promotion citing a symbol; a code-intelligence change-impact query run in the context of an active memory session). Compatibility: fully additive, no breaking change to either existing graph.

3. **Code-intelligence indexing is snapshot-whole, not file-incremental.** Current: `IndexSnapshot` takes a full file list per call (Inferred from the API shape, not exhaustively confirmed). Why it becomes a problem: a monorepo with tens of thousands of files re-parsing everything on every index call doesn't scale, even though the *storage* model (immutable snapshots) is already correctly designed for incrementality — it's the *indexing driver*, not the schema, that isn't incremental yet. Severity: Medium, scales with repo size. Trigger point: repos where a full snapshot re-index exceeds an acceptable indexing-latency budget (needs a benchmark, §16, to pin a number). Superior architecture: content-hash-gated per-file re-parse (skip files whose content hash matches the parent snapshot's `code_file_versions` row) feeding into the same `IndexSnapshot` call, or a new incremental entry point. Migration path: additive — the schema already supports it via `code_file_versions.content_hash` and `renamed_from_file_id`; this is a driver-logic change, not a schema change. Compatibility: fully backward compatible.

4. **Contradiction detection is a bag-of-words heuristic, general field gap, not SCOS-specific.** Already covered in depth, §2.3 and §8. Severity: Medium — false negatives (same-polarity factual conflicts) are the more dangerous failure mode than false positives here, since a missed contradiction silently corrupts trust in stored facts rather than just annoying a user with a false alarm. Superior architecture: at minimum, a semantic-similarity-plus-opposite-value check (two facts about the *same normalized key* with *different normalized values*, not just word-overlap) would catch same-polarity conflicts the current negation-word check misses entirely — this doesn't require new infrastructure, `insertFact`'s existing `fact_key_norm`/`fact_value_norm` normalization (referenced in the prior audit, not re-verified line-by-line in this pass but plausible given the schema) is very likely already sufficient input for a same-key-different-value check that's strictly better than the current negation-word heuristic, without needing an LLM call at all.

---

## 22. Recommended Target Architecture

Rather than a full from-scratch redesign (the existing service-mesh-plus-Postgres-plus-OKF shape is sound and none of the seven competitors inspected in this pass suggest a categorically different approach would be better — see §14's reasoning), the target architecture is the current one **plus** four concrete additions, each additive and independently shippable:

1. **Standing-answer cache** (§6.2): `memory_standing_answers(namespace_id, question_key, answer, evidence_fact_ids[], last_refreshed_at, refresh_reason)`, checked first by `retrieval-orchestrator` before full fan-out, refreshed by a background job triggered on relevant-fact writes.
2. **Cross-encoder rerank stage** (§6.4): an optional post-RRF stage in `retrieval-orchestrator`, backed by a small local model served alongside (or by extending) `workers/embeddings`, with the same explicit-fallback pattern already used for vector search.
3. **Memory↔code evidence bridge**: `memory_code_links(fact_id, code_symbol_version_id, link_type, confidence, created_at)` — link types minimally `evidences | motivated_by | invalidated_by`. Two write points: (a) OKF promotion, when the promoted content cites a specific symbol/change-impact result already in context; (b) a new `scos_link_evidence` MCP tool an agent can call explicitly when it knows a decision and a code change are related, rather than trying to infer the link automatically on day one (automatic inference is a §25 moat item, not a P0 — ship the explicit, agent-driven version first, since it requires zero new inference logic and immediately makes the link queryable).
4. **Graph-centrality code context ranking** (§6.7): a PageRank pass over `code_edges`, exposed as a new `code-intelligence` or `context-optimisation` endpoint, personalizable toward recently-changed/explicitly-referenced symbols the same way Aider personalizes toward chat-mentioned files.

Primary data flows (as they'd work with the above additions layered on; unmarked steps are already-shipped, Verified):

1. **Adding a memory** — turn arrives at `memory-gateway` → `memory-intelligence`'s `handleFacts` → `insertFact`/`createOrSupersedeFact` (transaction + advisory lock, Verified) → `findMergeCandidate` (heuristic shortlist, LLM-gated confirm) → `isContradictory` check → **[new]** same-key-different-value check → write → `maybePromoteToOKF` (LLM-gated) → **[new]** if the promotion cites a code symbol, write `memory_code_links`.
2. **Retrieving memories** — `retrieval-orchestrator`'s `handleRetrieve` → **[new]** standing-answer cache check (namespace + question-key match) → on miss, existing 5-source fan-out (exact/FTS/vector/relation/session) → RRF fusion → **[new]** cross-encoder rerank (fallback: existing linear boost) → token-budget allocation.
3. **Indexing a new repository** — `scos_code_repo_register` → `EnsureRepository` → `CreateSnapshot` → three-tier extraction (Go-native/Tree-sitter/SCIP as available) → `IndexSnapshot` writes symbols/files/edges → snapshot promoted to `active`.
4. **Processing a changed file** — currently: caller re-submits a full snapshot's file list. **[new, recommended]:** a content-hash-gated incremental path that only re-parses files whose hash changed since the parent snapshot, still producing a new immutable snapshot generation.
5. **Answering a code question** — `scos_code_query`/`scos_code_impact` → `QueryByPathOrFQN`/`ChangeImpact` (BFS over `code_edges`) → **[new]** PageRank-weighted ranking of the result set before returning it.
6. **Answering a question requiring both code and memory** — **not yet possible as a single call** (the current gap, §13/§21.2). Target: a new `retrieval-orchestrator` mode that queries both `memory_code_links` and the existing five memory sources, fusing an "evidences"/"motivated_by" code-linked result set in alongside the normal fusion — this is the concrete shape of the moat described in §25, and does not exist today.

---

## 23. Recommended Data Model (additive schema only — not a rewrite of existing tables)

```sql
-- Standing precomputed answers (§6.2)
CREATE TABLE memory_standing_answers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    namespace_id uuid NOT NULL,
    question_key text NOT NULL,           -- e.g. "user_preferences_summary"
    answer text NOT NULL,
    evidence_fact_ids uuid[] NOT NULL DEFAULT '{}',
    confidence float NOT NULL DEFAULT 0.5,
    last_refreshed_at timestamptz NOT NULL DEFAULT now(),
    refresh_reason text,                  -- what triggered the last rewrite
    UNIQUE (tenant_id, namespace_id, question_key)
);

-- Memory <-> code evidence bridge (§13, §21.2, §22)
CREATE TABLE memory_code_links (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    fact_id uuid NOT NULL,                -- references memory_facts-equivalent identity
    code_symbol_version_id uuid NOT NULL REFERENCES code_symbol_versions(id),
    link_type text NOT NULL CHECK (link_type IN ('evidences','motivated_by','invalidated_by')),
    confidence float NOT NULL DEFAULT 1.0,   -- 1.0 for explicit agent-asserted links (P0 path);
                                              -- <1.0 reserved for future inferred links (P2/moat path)
    source text NOT NULL CHECK (source IN ('explicit','okf_promotion','inferred')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (fact_id, code_symbol_version_id, link_type)
);
CREATE INDEX memory_code_links_symbol_idx ON memory_code_links (code_symbol_version_id);
CREATE INDEX memory_code_links_fact_idx ON memory_code_links (fact_id);
```

Both tables are strictly additive: no existing table is altered, RLS can be enabled on both following the exact pattern already established in `database/policies/000003_data_plane_rls.sql` (tenant + namespace-membership scoped), and both can be dropped without affecting any existing subsystem if either idea doesn't pan out in practice — a deliberately low-commitment way to test the two highest-value recommendations in this report before investing further.

---

## 24. Prioritized Engineering Roadmap

**P0 — foundation / near-zero-risk, high-value**

- **P0-0: Make RLS actually enforce.** Problem: `database/` sets no `FORCE ROW LEVEL SECURITY`, and both `deploy/compose/docker-compose.yml` and `deploy/native/postgres-setup.sh` connect services as the table-owning Postgres role, which is exempt from RLS regardless of policies present — verified against current HEAD, corroborating this repo's own `audit-results/2026-09-02/` (AUTH-001) and `audit-results/comparative-divergence-audit.md` (F-08). Evidence: direct grep of `database/` for `FORCE ROW LEVEL SECURITY`/`NOBYPASSRLS` (only found in the test fixture, `database/integration/rls_test.go:212`) plus both deployment scripts' role provisioning. Implementation: (1) provision a `NOSUPERUSER NOBYPASSRLS` application role in both deployment paths, distinct from the migration-owning role; (2) add `ALTER TABLE ... FORCE ROW LEVEL SECURITY` for every table currently RLS-enabled; (3) point every service's `DATABASE_URL`/connection config at the new restricted role, not the owner; (4) add a deployment-time (not just test-fixture) assertion that the connecting role lacks `BYPASSRLS`, so this can't silently regress. Affected: `database/policies/`, `deploy/compose/docker-compose.yml`, `deploy/native/postgres-setup.sh`, every service's DB connection config. New components: none. Tests: `database/integration/rls_test.go`'s existing fixture role should become the actual deployed role, not a parallel one — extend the same tests to run against the real deployment config. Complexity: S-M. Risk: Low (additive role + config change; existing app code already writes correctly-scoped queries, this only makes the database also enforce them). Impact: **Transformational** — this is the precondition for every RLS-related claim elsewhere in this document being true rather than aspirational. Prerequisites: none — do this before anything else in this backlog, including the other P0 items below.

- **P0-A: Graph-centrality code context ranking.** Problem: §6.7/§21. Evidence: Aider `repomap.py`, Verified. Implementation: PageRank over existing `code_edges`, personalized toward recently-changed/mentioned symbols. Affected: `services/code-intelligence` or `services/context-optimisation`. New components: none (uses existing graph). Dependencies: a graph library callable from Go (networkx is Python-only; use a small Go PageRank implementation or add it to a Python worker that already has `networkx`-equivalent access — check `workers/` for an existing Python graph dependency before adding one). Tests: rank-order regression tests against a fixed small repo fixture. Benchmarks: does ranked-first code context measurably improve change-impact-query relevance (§16's proposed benchmark). Complexity: S. Risk: Low. Impact: High. Prerequisites: none.

- **P0-B: Verify and, if absent, close the write-time PII/secret-scanning gap.** Problem: §6.6/§18. Evidence: Hindsight's Memory Defense (Documented); SCOS's own gap status is Unknown, not confirmed missing. Implementation: first, read `security-gate`'s actual source to resolve the Unknown; only build new scanning if genuinely absent. Complexity: XS (to check) + S (to build, if needed). Risk: Low. Impact: High if missing (safety-class gap), zero effort wasted if already present. Prerequisites: none — do this first, it's a research task before it's an engineering task.

- **P0-C: Same-key-different-value contradiction check.** Problem: §21.4. Implementation: extend `isContradictory` with a check against `fact_key_norm` equality + `fact_value_norm` inequality, using normalization logic that plausibly already exists (verify against `insertFact`'s actual normalization before assuming). Complexity: XS-S. Risk: Low. Impact: Medium-High (closes the most dangerous class of missed contradiction). Prerequisites: confirm `fact_key_norm`/`fact_value_norm` actually exist at current HEAD (referenced by the superseded prior audit against an older revision — re-verify, don't assume carried forward).

**P1 — major competitive gaps**

- **P1-A: Cross-encoder rerank stage.** §6.4/§21.1/§22. Complexity: M. Risk: Low (fully additive, fallback-safe). Impact: High. Prerequisites: a served local cross-encoder model (extend `workers/embeddings` or add a sibling worker).
- **P1-B: Standing-answer cache.** §6.2/§22/§23. Complexity: M. Risk: Low. Impact: High (latency + cost win on top of quality). Prerequisites: none beyond the new table.
- **P1-C: Explicit memory↔code evidence links.** §13/§21.2/§22/§23. Complexity: M. Risk: Low. Impact: Transformational if it's the seed of the full moat (§25), Medium standalone. Prerequisites: none — ship the explicit-link version first, inference comes later (P2/P3).
- **P1-D: Content-hash-gated incremental code indexing.** §21.3. Complexity: M. Risk: Medium (touches the core indexing driver). Impact: High for large-repo adopters specifically. Prerequisites: confirm current `IndexSnapshot` call shape precisely before designing the incremental variant (this pass inferred whole-snapshot re-indexing from the API signature, not from tracing every caller — verify first).
- **P1-E: First ADR-0010-compliant published benchmark.** §15/§16. Complexity: M-L (building the corpus is the real work). Risk: Low. Impact: High for credibility, zero product risk. Prerequisites: none — the harness already exists.

**P2 — differentiators**

- **P2-A: Ontology-guided entity extraction**, anchored on existing `MemoryNetwork` types. §6.5. Complexity: M. Risk: Medium (extraction-quality regression risk if the ontology is too rigid). Impact: Medium.
- **P2-B: Bitemporal schema extension** (`belief_expired_at` distinct from `valid_until`). §6.1. Complexity: S-M (schema + `createOrSupersedeFact` change). Risk: Low. Impact: Medium (correctness of historical-state queries, not user-visible day-to-day).
- **P2-C: Pluggable retrieval strategies** (caller-selectable, Cognee-style) rather than one fixed fan-out. §11/§20.9. Complexity: L. Risk: Medium (API surface change). Impact: Medium-High.
- **P2-D: Agent-execution-trace-specific consolidation** (Cognee's closest-to-SCOS's-actual-audience idea). §20.10. Complexity: M-L. Risk: Medium. Impact: High specifically for coding-agent use, which is SCOS's stated audience.

**P3 — advanced / moat**

- **P3-A: Automatic (inferred, not just agent-asserted) memory↔code linking**, building on P1-C's explicit-link foundation. §25. Complexity: XL. Risk: High (false-link risk if inference is weak — must ship with a confidence field and stay below 1.0, never silently promoted to explicit-confidence). Impact: Transformational.
- **P3-B: Surprisal-driven consolidation scheduling**, reimplemented from Honcho's *idea* (not code — AGPL). §6.3/§20.3. Complexity: XL (genuine research effort). Risk: High. Impact: Medium-High.
- **P3-C: Cross-repository code graph.** §9/§10. Complexity: L-XL. Risk: Medium. Impact: Medium (valuable mainly for multi-repo adopters, which may or may not be SCOS's near-term market — a product question, not just an engineering one).

---

## 25. Moat Opportunities — Ranked by Defensibility

1. **Memory↔code evidence bridge, explicit-first then inferred** (P1-C → P3-A). Defensibility: **High**. No competitor inspected in this pass has both planes at all, let alone a bridge between them — this isn't "do RRF better than Hindsight," it's a category of question (*why* does this code exist, grounded in the actual conversation that decided it) literally nobody else in the comparison set can answer, because nobody else has both a memory graph and a code graph in the same evidence model. Practical value: directly addresses real questions engineers ask ("why did we build it this way", "what decision does this code trace back to") that no pure-memory or pure-code-intelligence system can answer at all, regardless of how good its retrieval is within its own domain.

2. **Automatic stale-memory invalidation when code changes** (implied by P3-A once the bridge exists). Defensibility: **High**, contingent on #1 shipping first. Once a fact is linked to a code symbol version, a later `ChangeImpact` result touching that symbol is a natural trigger to flag the linked fact for review ("this decision's factual basis may no longer be true — the code it was about has changed"). This is a direct, mechanical consequence of the bridge existing, not a separate research project — sequence it right after P1-C, not as an independent later item.

3. **Graph-centrality context ranking, extended from code into memory** (P0-A, then generalized). Defensibility: **Medium** — Aider's technique is public and easy to copy once someone thinks to apply it to a memory graph too, but SCOS would be first among the memory-focused competitors inspected in this pass to do it, and being first with a cheap, explainable, no-LLM technique is a real if not permanent edge.

4. **Repository-specific learned retrieval** (a longer-horizon idea, not in the P0–P3 backlog above because it requires production usage data SCOS doesn't have yet): once real usage/feedback signal exists (`handleFactFeedback`'s POSITIVE/NEGATIVE/VERY_NEGATIVE, currently recorded but — verified — not fed back into ranking automatically), close that loop. Defensibility: **Medium-High** long-term, but explicitly sequenced after there's enough real feedback volume to learn from — premature to build before that data exists.

5. **Self-improving retrieval from agent outcomes** (did the agent actually use/rely on what was retrieved, verified against its subsequent actions). Defensibility: **High** in principle, **High difficulty and High risk** in practice (requires reliable outcome attribution, which is a hard, unsolved-in-general problem) — listed last deliberately, as the most speculative item in this report, not because it's least valuable if it worked.

---

## 26. Definition of "Best on the Market"

Based on everything verified in this pass, "best on the market" for SCOS specifically should **not** mean matching every point-feature of every competitor (that produces the "large, expensive imitation" the prior audit already correctly warned against, and this pass's own findings reinforce that warning independently — every competitor inspected has a real, different center of gravity: Mem0 optimizes LLM-call efficiency, Graphiti optimizes temporal correctness, Honcho optimizes consolidation sophistication, Cognee optimizes pipeline composability, Hindsight optimizes product polish and published proof, none of them optimize for code-plus-memory unification because none of them have a code plane). "Best on the market," for SCOS, concretely means: **the only system that can correctly and verifiably answer a question spanning both a project's human decision history and its code's actual current state, with database-enforced tenant isolation and a published, reproducible benchmark result to back the claim.** Every other dimension in this report (rerank quality, bitemporal precision, consolidation sophistication) is worth closing because it's currently a real weakness, not because it's the basis of the eventual competitive claim.

---

## 27. Final Recommended Build Order

1. Resolve the two Unknowns that block confident prioritization elsewhere in this report: (a) does `security-gate` already scan for PII/secrets at write time (§6.6/§24 P0-B), (b) does `fact_key_norm`/`fact_value_norm` normalization still exist at current HEAD the way the superseded prior audit described it against an older revision (§24 P0-C). Both are read-only research tasks, hours not days, and several other recommendations' cost estimates depend on their answers.
2. Ship P0-A (graph-centrality code ranking) and P0-C (same-key-different-value contradiction check) — both low-risk, high-value, additive, and buildable in parallel by different people since they touch different services.
3. Ship P1-C (explicit memory↔code evidence links) as its own small, self-contained feature — do not wait for or bundle it with P1-A/P1-B, since it's the seed of the entire §25 moat and the sooner it exists (even in a minimal, agent-asserted form with no inference), the sooner real usage data starts accumulating for P3-A/§25-item-4 to eventually build on.
4. Ship P1-A (cross-encoder rerank) and P1-B (standing-answer cache) — both meaningfully improve retrieval quality/latency independent of everything else, low risk, fully additive.
5. Run P1-E (the first ADR-0010-compliant published benchmark) as soon as P1-A/P1-B are in, so the published number reflects the improved system rather than needing to be rerun immediately after.
6. Only then take on P2/P3 — they're real, but every one of them is higher-risk, higher-effort, or dependent on data/usage that doesn't exist yet, and none of them are blocking anything else in this list.

---

*End of audit. Every claim above is tagged Verified/Documented/Inferred/Unknown at the point it's made; where a section's synthesis draws on several such claims, the tag reflects the weakest link in that synthesis, not the strongest. This document does not carry forward any claim from the superseded same-day audit without independently re-checking it against current HEAD.*
