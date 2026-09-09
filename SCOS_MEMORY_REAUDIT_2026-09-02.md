# SCOS Memory Re-Audit — 2026-09-02

> Historical re-audit snapshot. The repository has moved since this report;
> treat its commit hashes and implementation status as historical. Current
> evidence is maintained under [`audit-results/`](audit-results/).

**Repository:** `dylmarriner/scos-memory`  
**Previous audit baseline:** `78755c40b2ba36300313da1ba5c5df029ff4aab6`  
**Re-audit head:** `1898f0453f37d9c3166f598c2193622357d2b6cb`  
**Delta:** 18 commits ahead of the original audit baseline  
**Purpose:** Re-test the original competitive audit findings against the implementation that landed after the audit and technical blueprint.

---

## 1. Executive verdict

The repository has moved materially since the first audit. The original conclusion that SCOS had a substantive memory plane but a weak native code-intelligence plane is no longer sufficient.

The updated codebase now has a **credible code-intelligence foundation**:

- PostgreSQL-backed immutable repository snapshots;
- branch-aware snapshot activation;
- stable conceptual file/symbol tables separated from per-snapshot versions;
- exact symbol/path lookup;
- PostgreSQL full-text lexical search;
- real Go AST declaration extraction;
- syntactic Go call-edge generation with explicit confidence classes;
- graph-backed change-impact traversal;
- SCIP protobuf ingestion as a deterministic semantic-symbol source;
- Tree-sitter syntax fallback for Python, TypeScript, JavaScript and Rust;
- snapshot diff classification;
- tenant-scoped v2 code APIs;
- per-user durable-store opt-out;
- Graphify cache invalidation based on repository content state;
- release SHA/build metadata and deploy parity checks;
- expanded tenant-isolation regression coverage.

Those are not documentation-only changes. The relevant migrations, source implementations and integration tests are present.

However, SCOS is **not yet at Sourcegraph/SCIP, Joern, Hindsight or Graphiti depth**. Several of the original gaps have moved from “missing” to “partial,” but the difficult parts are now exposed more clearly:

1. SCIP ingestion currently imports declared symbols but explicitly does **not** ingest reference/call occurrences.
2. Tree-sitter provides syntax declarations only, not semantic definitions/references.
3. Go call edges are syntax-derived, same-package and only accepted when a bare call name resolves uniquely.
4. The new snapshot diff machinery does not yet constitute a true incremental index planner that automatically skips unchanged parsing/indexing work.
5. `/v1` code-intelligence routes still use the old in-memory store; `/v2/code/*` is the durable path.
6. Code retrieval is exact-first then lexical fallback, not a federated exact + lexical + dense + graph + temporal retrieval fabric.
7. Code embeddings, graph fusion, RRF, reranking and query planning are not yet present in the new native code path.
8. The memory plane still lacks the full bitemporal/observation/mental-model semantics needed to match Graphiti/Hindsight-class temporal memory.
9. Cross-code/memory provenance, decision-to-commit lineage and code-driven memory invalidation remain the major moat opportunity, not an implemented capability.

**Revised assessment:** SCOS has crossed the line from “broad platform with an underdeveloped code subsystem” to **“broad platform with a legitimate code-intelligence architecture that now needs semantic depth, retrieval fusion and temporal unification.”**

---

## 2. What changed since the original audit

The re-audit baseline comparison shows 18 commits between the original pinned revision and current head. The important implementation commits are:

- `e643b67668a553ce82e2e688461644bfda33b491` — Graphify cache staleness fix.
- `c91874d3413803ae4265a2a4187765b3c1ba037c` — release parity/version manifest.
- `9854a3c7ea3e154cee68b8bf7fdac769ee9aa997` — extension-manager IDOR fix plus tenant regression sweep.
- `9bade7c1c729731d409c78e362b8d35a5e65b3b0` — code-intelligence ground-truth fixtures.
- `35c5d3c15e15268dde4bbfaebdc4a6e13a2ce1a7` — durable snapshot-based code store.
- `578cadb28a0c03a0ecb24c78792c3d6a96358e21` — deploy pipeline migration execution fix.
- `a0b8e9159d06512572c785e3e247b08fb024f1ae` — lexical FTS code search.
- `48f7a6edc7717432bb7fcef1296c1c43e1237ee2` — snapshot parent linkage and diffing.
- `5fd1ae684ff752467c8f4382cc28f4daf0ef10ac` — durable code store made mandatory at infrastructure level with per-user opt-out.
- `1356ccd790704f2d3f65e578902ea5cb9bf95d9e` — real Go declaration extraction via `go/parser`.
- `b9a9c1c9e2193a8eed9e85b3fd54660c01f2a8ce` — Go syntactic CALLS edges.
- `059ba026824de73db0adf702f480a9cc0c77c67e` — graph-backed change impact.
- `64c63b727b7cb65d5f2f56d86dcf4499781c46a9` — SCIP ingestion.
- `7cf525cb0086557f917b9ee628b14b72542675e2` — parity digest comparison fix.
- `1898f0453f37d9c3166f598c2193622357d2b6cb` — Tree-sitter syntax fallback.

This is a meaningful implementation wave, not cosmetic roadmap churn.

---

## 3. Original P0 status

| Original P0 | Re-audit status | Assessment |
|---|---|---|
| Replace JSON/map code store | **Mostly closed for v2** | PostgreSQL snapshot store exists and is always configured; legacy v1 remains in-memory. |
| Real parser/semantic indexing | **Partial / strong progress** | Go parser + SCIP + Tree-sitter now exist. SCIP references/calls are not ingested yet. |
| Canonical typed code graph | **Partial** | `code_edges` exists with relation/confidence/extractor/source metadata, but edge families remain shallow. |
| True incremental/Git-aware indexing | **Partial** | Immutable snapshots, parents and diff classification exist. Automatic changed-file-only planning is still missing. |
| Federated retrieval | **Partial** | Exact + FTS lexical exists. Dense/graph/temporal fusion/RRF not implemented in native code search. |
| Snapshot-aware cache invalidation | **Closed for Graphify** | Cache key now includes content snapshot, extractor version and backend. |
| Reproducible benchmark baseline | **Partial** | Ground-truth fixtures and live-DB integration tests exist; competitive benchmark suite remains incomplete. |

---

## 4. Durable snapshot store — verified improvement

**Evidence:**

- `database/migrations/000025_code_intelligence_persistence.sql`
- `services/code-intelligence/cmd/server/pgstore.go`
- `services/code-intelligence/cmd/server/pgstore_integration_test.go`

The new schema introduces:

- `code_repositories`
- `code_repository_snapshots`
- `code_files`
- `code_file_versions`
- `code_symbols`
- `code_symbol_versions`
- `code_chunks`
- `code_edges`
- `code_impact_reconciliations`

The architecture correctly separates conceptual identity from snapshot occurrence.

`code_repository_snapshots` is keyed by:

`repository + branch + commit + extractor_version + configuration_hash`

and activation is performed through `activate_code_snapshot()` so only the active snapshot for the same repository/branch is superseded.

This closes two concrete failures from the original code store:

- branch/commit state no longer has to overwrite one mutable repository state;
- rename lineage can be represented across snapshots.

### Remaining concern: v1/v2 split

`services/code-intelligence/cmd/server/baseline_fixtures_test.go` explicitly documents that `/v1` still uses the old in-memory `codeStore` and still loses rename lineage / prior commit state.

The new architecture is exposed through `/v2/code/*`.

This is acceptable as a migration strategy, but the audit should not claim the old path has been replaced until callers are actually migrated and the legacy store is retired.

**Priority:** migrate all first-party callers to v2, then delete or hard-deprecate the old stateful implementation.

---

## 5. New correctness issue found: snapshot symbol count is inaccurate

In `pgCodeStore.IndexSnapshot`, the snapshot update currently sets:

```sql
file_count = len(files)
symbol_count = len(files)
```

That is wrong once parser-derived declarations are stored alongside file-level symbols.

A file containing 20 declarations still increments `symbol_count` as one.

SCIP indexing correctly uses the imported symbol count, so the metric has inconsistent semantics between indexing paths.

**Severity:** Medium correctness / observability.

**Fix:** calculate actual inserted symbol-version count within the transaction and persist that value. Add a regression test with one file containing multiple declarations.

---

## 6. Go AST extraction — verified and useful, but not semantic resolution

**Evidence:** `services/code-intelligence/cmd/server/goextract.go`

The new Go extractor uses:

- `go/parser`
- `go/ast`
- `go/printer`
- source spans
- declaration-local hashes
- doc comments
- real function/method/type/const/var declarations

This is a substantial improvement over filename-convention extraction.

It correctly avoids pretending to be type-aware. The implementation itself states that it does not run a `go/types` checker over a loaded package.

### Strengths

- real source ranges;
- real signatures;
- declaration-level content hashing;
- unrelated declarations remain stable when another function body changes;
- doc comments survive indexing;
- call names are extracted from actual AST call expressions.

### Remaining limits

- no type resolution;
- no cross-package target resolution;
- selector calls are reduced to their bare method/function names;
- overload-like ambiguity is resolved by omitting edges rather than guessing, which is the correct conservative behaviour but limits graph recall;
- imports/references/implementations are not represented by this tier.

This should remain the **syntax/static-analysis fallback for Go**, while SCIP or a type-checked Go index becomes the preferred semantic source.

---

## 7. SCIP ingestion — major competitive improvement, but only half-used

**Evidence:** `services/code-intelligence/cmd/server/scipimport.go`

SCOS now decodes standard SCIP protobuf indexes through the official Go bindings and imports declared symbols into durable snapshots.

Imported symbols carry:

- SCIP-derived stable key;
- path;
- kind;
- display name/FQN;
- signature documentation;
- doc comments;
- `confidence_class = deterministic`;
- extractor provenance.

This is strategically correct. SCOS should consume mature compiler/indexer output instead of recreating every compiler frontend.

### Critical remaining gap

The source explicitly says **reference and call-graph occurrences are deliberately not ingested**.

That means SCOS currently consumes the symbol table portion of SCIP but not the part that gives SCIP most of its competitive value for code navigation:

- references;
- definitions linked from occurrences;
- implementations where emitted;
- semantic relationship edges.

SCIP exists specifically to power go-to-definition, find-references and find-implementations class workflows. SCOS now speaks the format, but it does not yet exploit the full graph.

### Next implementation

Implement occurrence-to-enclosing-symbol attribution:

1. persist definition ranges;
2. build an interval structure per document;
3. for every non-definition occurrence, identify the smallest enclosing source symbol;
4. resolve the target SCIP symbol;
5. create `REFERENCES`, `CALLS` or more specific relation edges where the SCIP role/kind permits;
6. store source range and deterministic provenance;
7. test cross-file and cross-package references.

This is the single highest-leverage code-intelligence follow-up.

---

## 8. SCIP location/provenance gap

`indexSCIPSymbolLocked` stores the document path, kind, signature and documentation, but it does not currently persist the precise definition source range from SCIP occurrences into the symbol version.

That weakens:

- exact jump-to-definition responses;
- provenance citations;
- overlap/containment used to attribute references to enclosing symbols;
- context extraction around definitions.

**Priority:** persist definition occurrence ranges before implementing reference edges.

---

## 9. Tree-sitter fallback — verified, correctly scoped

**Evidence:** `services/code-intelligence/cmd/server/treesitter.go`

Tree-sitter grammars are now wired for:

- Python;
- TypeScript;
- JavaScript;
- Rust.

The extractor records syntax-derived declarations with ranges and hashes and explicitly labels itself as syntax rather than semantic truth.

That is the right architecture.

### Remaining concerns

#### 9.1 Parse failure visibility

`indexTreeSitterDeclarationsLocked` intentionally swallows Tree-sitter errors and leaves the Tier-0 file symbol in place.

That is operationally resilient but under-observable. A snapshot may become `ready` even though syntax extraction failed for one or more files, with no durable extraction-warning record.

Add per-file extraction status:

- `success`
- `fallback`
- `parse_error`
- `unsupported`

and expose counts on the snapshot.

#### 9.2 Signature quality

Tree-sitter currently stores the entire declaration body in the `Signature` field.

For large functions/classes this is not a signature and can inflate query responses/context.

Generate concise declaration headers separately from body/chunk content.

#### 9.3 Symbol identity

The Tree-sitter stable key combines language, FQN and content hash. A rename changes the FQN and usually changes the declaration source as well, so symbol identity across semantic renames is not preserved by this tier.

That is acceptable for a syntax fallback, but rename lineage should be determined by a dedicated lineage pass, not assumed from stable keys alone.

---

## 10. Call graph — moved from missing to partial

**Evidence:** `insertGoCallEdgesLocked` in `pgstore.go`.

Go function/method calls now produce `CALLS` edges when a bare syntactic target name has exactly one function/method candidate in the same package.

Edges include:

- snapshot;
- source symbol version;
- target symbol version;
- relation;
- confidence class;
- extractor.

This is honest static analysis.

It is not yet a production semantic call graph because it cannot correctly resolve:

- methods by receiver type;
- imported package calls;
- interface dispatch;
- function values;
- generic instantiations;
- cross-package calls;
- external dependencies.

SCIP references should become the preferred semantic edge source; Go syntax edges can remain as lower-confidence fallback evidence.

---

## 11. Change-impact analysis — real now, but narrow

**Evidence:** `pgCodeStore.ChangeImpact`.

The durable implementation resolves a symbol and walks incoming `code_edges` up to a bounded depth.

This is a real graph traversal, unlike the old endpoint whose underlying relation field was effectively empty.

Current impact meaning is essentially:

> callers of this symbol, callers of those callers, up to `maxDepth`.

That is useful but only one dimension of engineering impact.

To become competitive, add:

- imports/dependency edges;
- type/interface implementation edges;
- schema/API consumers;
- test-to-symbol links;
- config/runtime dependencies;
- Git co-change history;
- historical reconciliations.

The existing `code_impact_reconciliations` table remains a promising seed for outcome-based scoring.

---

## 12. Snapshot diffing — useful but not yet true incremental indexing

**Evidence:** `CreateSnapshot`, `DiffSnapshot`, `/v2/code/snapshots/diff`.

Parent snapshot linkage and Added/Modified/Removed/Renamed classification are real improvements.

However, the current HTTP indexing path still accepts a `files[]` collection and `IndexSnapshot` iterates over every supplied file.

The diff is calculated **after** indexing and returned to the caller.

Therefore the current implementation can tell a caller that only one file changed, but it does not yet automatically guarantee that only that file was parsed/indexed.

### Required next step

Create an **incremental index planner** before extraction:

1. resolve parent snapshot;
2. compare Git tree/content hashes before parsing;
3. carry forward unchanged file/symbol/edge projections into the new snapshot without re-extraction;
4. parse only changed/new files;
5. tombstone removed files/symbols;
6. repair only impacted edges;
7. embed only changed chunks;
8. validate and activate.

The completion gate should measure parser work, not merely diff output.

Example assertion:

`1000-file repo + one changed file -> parser_invocations <= affected_files + dependency_rechecks`

---

## 13. Lexical retrieval — closed as a baseline, not yet hybrid

**Evidence:** migration `000026_code_lexical_search.sql` and `QueryLexical`.

SCOS now uses PostgreSQL generated `tsvector` data with a GIN index and normalizes code-path separators before tokenization.

The query path is intentionally:

1. exact path/FQN search;
2. if no exact result, lexical search.

That is a sensible deterministic baseline.

It is not yet the target retrieval fabric from the blueprint.

Still missing in native code retrieval:

- dense symbol/chunk retrieval;
- graph expansion as a retriever;
- temporal/Git retrieval;
- RRF fusion;
- reranking;
- diversity controls;
- token-budgeted code context selection;
- retrieval traces specific to code candidate fusion.

Hindsight's current memory recall pipeline remains materially ahead in retrieval composition: semantic + BM25 + graph + temporal retrieval, RRF, cross-encoder reranking and token filtering.

---

## 14. Graphify staleness finding — closed

**Evidence:** `services/graphify-provider/cmd/server/graphify.go`.

The original Critical finding was valid at baseline: cache existence was checked without source staleness.

The updated implementation fingerprints:

- cleaned repository path;
- Git HEAD;
- uncommitted/untracked content state;
- Graphify version;
- backend configuration.

Dirty Git files are content-hashed, so an uncommitted edit changes the cache identity.

This is a substantive correctness fix.

### Remaining operational issue

Old immutable cache generations are not garbage-collected.

Add retention based on:

- active snapshots;
- recent access;
- max generations/repository;
- disk budget.

This is now an operational/storage issue, not a correctness blocker.

---

## 15. Deployment parity — materially improved

`packages/release` now provides build/source provenance and services expose runtime version information.

The auto-deploy path checks running image parity against the freshly built image, and the digest-prefix bug discovered during real deployment was subsequently fixed.

The migration runner was also corrected so newly added SQL migrations are actually re-executed on existing deployments before new service code starts.

This closes a major class of source-vs-production drift from the original audit.

### Still recommended

Extend the release manifest beyond source SHA/build time to include:

- schema version;
- index format version;
- extractor version bundle;
- configuration fingerprint.

A service can be on the right source commit and still be incompatible with the database/index generation it is serving.

---

## 16. Tenant isolation — stronger than baseline

The updated repository contains a real fix for an extension-manager cross-tenant IDOR and a wider regression-test sweep enforcing the principle that authenticated claims override spoofable body/header tenant values.

This is an important improvement because it demonstrates the isolation model was tested as an adversarial invariant rather than inferred from middleware presence.

Remaining security work from the original audit still stands:

- derived-index deletion proofs;
- backup retention/deletion propagation;
- cache namespace/auth scope verification;
- explicit encryption semantics for embeddings/graphs;
- systematic authorization tests for every new v2 code route and future graph retriever.

---

## 17. Memory plane re-assessment

The post-audit commit wave focused heavily on code intelligence and deployment integrity. The original memory assessment therefore changes less.

SCOS still has strong foundations in:

- transactional durable facts;
- normalized identity;
- supersession lineage;
- forgetting;
- TTL;
- provenance to sessions/turns;
- confidence/importance;
- merge handling;
- access tracking;
- autonomous pre-turn recall/post-turn persistence integrations.

The major competitive memory gaps remain:

- full valid-time + transaction-time bitemporality;
- explicit contradiction sets;
- evidence-backed observation layer;
- mental/project models;
- canonical entity resolution graph;
- semantic + sparse + graph + temporal fusion in one memory retrieval pipeline;
- learned/outcome-aware retrieval;
- automatic code-driven staleness of memories.

Graphiti remains stronger in explicit bitemporal graph semantics. Hindsight remains stronger in integrated multi-strategy recall and reflection/observation behaviour. Honcho remains stronger in observer/peer representations.

---

## 18. Updated competitor position

### Versus Hindsight

SCOS is stronger in product breadth, code/repository awareness, control-plane integration and the potential to connect project history to code.

Hindsight remains stronger in memory retrieval composition and mature memory reasoning: four-way semantic/BM25/graph/temporal recall, RRF, reranking and reflection-oriented abstractions.

### Versus Graphiti

SCOS now has a much stronger code temporal/snapshot foundation than at the first audit, but Graphiti remains ahead in explicit bitemporal semantic relationships and temporal graph reasoning.

### Versus Honcho

SCOS remains broader technically; Honcho remains more advanced in peer/observer-centric social memory and perspective modelling.

### Versus Sourcegraph / SCIP ecosystem

The gap has narrowed structurally because SCOS can now ingest SCIP.

But Sourcegraph-class code navigation depends on the very SCIP occurrence/reference graph SCOS does not yet import.

Current SCOS status is therefore:

**SCIP-compatible symbol ingestion, not yet SCIP-complete code navigation.**

### Versus Joern/CPG systems

SCOS remains far behind in control-flow/data-flow/program-analysis depth. This should stay a later phase until semantic references and hybrid retrieval are finished.

### Versus Aider repo-map

SCOS now has richer durable symbols/graphs, but query-conditioned graph centrality and token-budgeted repo-map selection remain missing.

---

## 19. Revised capability matrix

| Capability | Original audit | Re-audit |
|---|---|---|
| Durable code persistence | Weak | **Strong foundation** |
| Immutable snapshots | Missing | **Implemented** |
| Branch coexistence | Missing | **Implemented** |
| Rename-aware file identity | Missing | **Implemented for content-identical file moves** |
| Real Go parsing | Missing | **Implemented** |
| Broad syntax parsing | Weak | **Tree-sitter fallback implemented** |
| SCIP ingestion | Missing | **Implemented for declarations** |
| SCIP references | Missing | **Still missing** |
| Exact symbol lookup | Basic | **Implemented on durable store** |
| Lexical code search | Missing | **Implemented** |
| Dense code retrieval | Missing | **Still missing** |
| RRF/fusion | Missing | **Still missing** |
| Call graph | Missing/basic | **Partial, Go syntax-derived** |
| Change impact | Partial/non-functional graph | **Real but CALLS-only** |
| Snapshot diff | Missing | **Implemented** |
| True changed-file-only indexing | Missing | **Still partial/missing planner** |
| Graphify staleness | Critical defect | **Fixed** |
| Release parity | Weak | **Substantially improved** |
| Tenant isolation regression coverage | Partial | **Improved materially** |
| Bitemporal memory | Missing | **Still missing** |
| Unified memory+code evidence graph | Missing | **Still target architecture** |
| Code-driven memory invalidation | Missing | **Still missing** |

---

## 20. New highest-priority work

The original roadmap should now be re-ordered based on what has actually landed.

### P0-1 — Complete SCIP semantics

Ingest definition ranges and occurrences, then create deterministic reference/call/implementation edges.

This gives the largest immediate jump in code correctness.

### P0-2 — Build the actual incremental index planner

Move diffing before extraction and carry unchanged projections forward without reparsing.

Measure parser/indexer invocations.

### P0-3 — Retire legacy v1 code state

Migrate all first-party callers to `/v2/code/*`, provide compatibility translation if required, then remove the mutable JSON/map runtime path.

Running two conceptual code stores indefinitely is architectural debt and a source of inconsistent answers.

### P0-4 — Fix index metrics and extraction health

Correct `symbol_count`, persist per-file extractor status and expose parser/index coverage.

### P0-5 — Native code retrieval fusion

Add:

- dense code embeddings/chunks;
- graph retriever;
- Git/temporal retriever;
- RRF;
- ranking traces.

Do this before a cross-encoder reranker.

### P0-6 — Stable symbol lineage engine

Content-hash identity handles some moves but not semantic renames.

Implement lineage scoring using:

- parent snapshot;
- AST shape;
- enclosing scope;
- signature similarity;
- body similarity;
- Git rename/diff hunks;
- SCIP symbol relationships where available.

### P0-7 — Link code evidence to memory evidence

Create the first real Unified Evidence Graph edges between:

- decisions;
- tasks;
- commits;
- symbol versions;
- memories.

This is where SCOS begins building its actual moat instead of merely catching up on code indexing.

---

## 21. Recommended next 10 commits

1. `feat(code-intelligence): persist SCIP definition occurrence ranges`
2. `feat(code-intelligence): ingest SCIP reference occurrences into deterministic edges`
3. `feat(code-intelligence): add incremental index planner and unchanged projection carry-forward`
4. `fix(code-intelligence): report actual snapshot symbol counts and extraction coverage`
5. `feat(code-intelligence): add symbol lineage matcher across snapshots`
6. `feat(code-intelligence): migrate first-party code callers from v1 to v2`
7. `refactor(code-intelligence): remove legacy mutable runtime store`
8. `feat(retrieval): add dense code-chunk retrieval and canonical candidate model`
9. `feat(retrieval): add exact+lexical+dense+graph+git RRF fusion with traces`
10. `feat(evidence): link decisions/tasks/commits/symbol versions through canonical provenance edges`

After those, begin bitemporal memory migration and code-driven memory revalidation.

---

## 22. Revised market-readiness scorecard

These scores are architectural maturity estimates, not benchmark claims.

| Plane | Baseline audit | Re-audit | Market-leading target |
|---|---:|---:|---:|
| Durable memory lifecycle | 7/10 | 7/10 | 9+/10 |
| Temporal memory semantics | 4/10 | 4/10 | 9+/10 |
| Code persistence/snapshots | 2/10 | 7/10 | 9+/10 |
| Syntax extraction | 3/10 | 7/10 | 9/10 |
| Semantic code navigation | 2/10 | 5/10 | 9+/10 |
| Code graph depth | 2/10 | 4/10 | 9/10 |
| Incremental indexing | 2/10 | 5/10 | 9+/10 |
| Code retrieval | 3/10 | 5/10 | 9+/10 |
| Change impact | 3/10 | 5/10 | 9/10 |
| Provenance/explainability | 6/10 | 7/10 | 10/10 |
| Deployment integrity | 5/10 | 8/10 | 9+/10 |
| Tenant/security engineering | 6/10 | 7.5/10 | 9+/10 |
| Unified memory+code intelligence | 3/10 | 4/10 | 10/10 |
| Benchmark defensibility | 3/10 | 4.5/10 | 9+/10 |

The biggest gain is code persistence/snapshot architecture. The biggest remaining gap is semantic relationship depth and unified retrieval.

---

## 23. Definition of the next milestone

The next milestone should be narrower and harder than “implement more roadmap items.”

SCOS should be able to index its own repository at two adjacent commits and prove all of the following:

1. unchanged files are not reparsed;
2. changed declarations receive new versions;
3. unchanged symbols preserve identity;
4. semantic references from SCIP resolve across files/packages;
5. exact, lexical, dense, graph and Git retrieval operate through one candidate model;
6. RRF improves benchmark recall over every individual retriever;
7. an impact query returns callers, references, tests and dependency evidence rather than CALLS-only reachability;
8. every result includes source snapshot, path/range, extractor and confidence;
9. legacy v1 and v2 cannot disagree because there is one canonical code store;
10. benchmark results are persisted against the exact source SHA and index format.

Only after that milestone should deep CPG/data-flow work move forward.

---

## 24. Strategic conclusion

The updated repo has executed a surprisingly large part of the first roadmap wave in a short number of commits. More importantly, it implemented the right **foundations** rather than merely creating placeholder services:

- durable snapshots;
- real parsers;
- confidence classes;
- standard semantic-index ingestion;
- graph edges;
- regression fixtures;
- deployment provenance.

The original P0 architecture emergency is therefore reduced.

The challenge has changed.

SCOS now needs to stop thinking in terms of “does code intelligence exist?” and start measuring **how correct and useful it is**.

The immediate competitive target should be:

> **SCIP-complete semantic relationships + genuinely incremental indexing + hybrid code retrieval.**

Then the project should pivot back to its unique advantage:

> **link persistent autobiographical/project memory to commit-aware code evidence so changes in code can automatically update, invalidate or explain remembered knowledge.**

That remains the capability most likely to make SCOS difficult to copy rather than merely feature-complete.
