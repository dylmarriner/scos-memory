# SCOS Memory Technical Blueprint and Engineering Roadmap

**Project:** SCOS Memory  
**Purpose:** Production-grade persistent intelligence, code intelligence, temporal knowledge graph, retrieval and context infrastructure for autonomous AI agents  
**Status:** Target architecture and implementation roadmap  
**Primary principle:** One canonical evidence model, multiple disposable retrieval projections

---

# 1. Mission

SCOS Memory should become the persistent intelligence substrate underneath AI agents.

Its job is not merely to store facts.

SCOS must continuously maintain an evidence-backed model of:

- users;
- agents;
- projects;
- conversations;
- decisions;
- requirements;
- tasks;
- repositories;
- branches;
- commits;
- files;
- symbols;
- dependencies;
- tests;
- APIs;
- schemas;
- incidents;
- runtime observations;
- and the relationships between all of them.

An agent using SCOS should be able to answer both:

> What is true now?

and:

> Why is it true, where did that knowledge come from, when was it true, what changed it, and what evidence supports it?

The long-term differentiator is the connection between **memory and evolving code**.

A project decision must be able to link to the conversation that created it, the task implementing it, the pull request and commits that changed the code, the symbols affected by those commits, and later changes that may invalidate the original decision or memory.

---

# 2. Non-Negotiable Architecture Principles

## 2.1 Canonical evidence first

PostgreSQL should remain the authoritative source for durable structured knowledge.

Vector indexes, lexical indexes, graph projections, caches and repository maps are derived views.

They must be rebuildable.

Never allow a vector database, graph database or search engine to silently become a second canonical truth store.

## 2.2 Immutable source evidence

Raw evidence should be append-only wherever practical.

Examples:

- conversation turns;
- source documents;
- Git commits;
- extracted repository snapshots;
- agent actions;
- retrieval traces;
- benchmark runs;
- runtime observations.

Derived conclusions may change.

The evidence supporting them must remain traceable.

## 2.3 Explicit time

Every durable knowledge assertion that can change should support both:

**Valid time**

When the claim was true in the world.

**Transaction time**

When SCOS learned or recorded the claim.

This allows SCOS to distinguish:

> “The API used port 8080 in March.”

from:

> “SCOS discovered in June that the API used port 8080 in March.”

## 2.4 Deterministic facts outrank inferred facts

Compiler-derived code relationships should outrank LLM guesses.

Git history should outrank inferred change dates.

Explicit user statements should be distinguishable from model-generated conclusions.

Every edge or claim must include an epistemic class.

Recommended classes:

- `deterministic`
- `source_explicit`
- `static_analysis`
- `heuristic`
- `model_inferred`
- `agent_observed`

## 2.5 Every answer must remain explainable

Retrieved context should carry:

- source ID;
- evidence IDs;
- timestamp;
- confidence;
- retrieval method;
- ranking contribution;
- snapshot or commit;
- extractor version where applicable.

SCOS should be able to explain why an item entered an agent's context.

## 2.6 Repository snapshots are immutable

Code intelligence must operate against explicit repository snapshots.

Never make “whatever happens to be on disk right now” the durable identity of indexed code.

A snapshot should identify:

`repository + commit + extractor version + indexing configuration`

## 2.7 No feature counts without runtime proof

An integration is not “supported” because configuration files exist.

A feature is complete only when its real runtime path has been exercised.

That principle should be extended from agent integrations to memory, retrieval, graph generation and code intelligence.

---

# 3. Target System Architecture

```text
                         ┌──────────────────────────┐
                         │       AI AGENTS          │
                         │ Claude / Codex / Hermes  │
                         │ OpenCode / MCP / Future  │
                         └────────────┬─────────────┘
                                      │
                                      ▼
                         ┌──────────────────────────┐
                         │   Context / Agent API    │
                         │ auth • policy • tenancy  │
                         └────────────┬─────────────┘
                                      │
                                      ▼
                   ┌──────────────────────────────────┐
                   │      RETRIEVAL ORCHESTRATOR      │
                   │                                  │
                   │ exact │ lexical │ dense │ graph  │
                   │ time  │ Git     │ metadata       │
                   └──────┬───────────┬───────────────┘
                          │           │
                 ┌────────▼───┐   ┌───▼────────────┐
                 │ Rank/Fusion│   │ Context Builder │
                 │ RRF/Rerank │   │ Token budgeting│
                 └──────┬─────┘   └─────┬──────────┘
                        │               │
                        └───────┬───────┘
                                ▼
                ┌──────────────────────────────────┐
                │   UNIFIED TEMPORAL EVIDENCE     │
                │             GRAPH                │
                │                                  │
                │ memory • people • decisions      │
                │ tasks • repos • commits • code   │
                │ provenance • temporal lineage    │
                └───────────────┬──────────────────┘
                                │
           ┌────────────────────┼─────────────────────┐
           ▼                    ▼                     ▼
 ┌─────────────────┐  ┌──────────────────┐  ┌────────────────┐
 │ PostgreSQL      │  │ Object Storage   │  │ Derived Indexes│
 │ canonical data  │  │ raw artifacts    │  │ FTS/vector/etc │
 └─────────────────┘  └──────────────────┘  └────────────────┘
                                ▲
                                │
                 ┌──────────────┴──────────────┐
                 │       INGESTION PLANE      │
                 ├─────────────────────────────┤
                 │ conversations              │
                 │ documents                  │
                 │ Git repositories           │
                 │ SCIP/LSP/compiler indexes  │
                 │ Tree-sitter                │
                 │ runtime events             │
                 │ agent actions              │
                 └─────────────────────────────┘
```

---

# 4. The Unified Evidence Graph

The Unified Evidence Graph, UEG, should become the conceptual center of SCOS.

This does **not** mean every query must run through Neo4j or another dedicated graph engine.

The graph is a data model.

Its canonical representation can initially live in PostgreSQL.

## Core node families

### Memory and knowledge

- Memory
- MemoryVersion
- Claim
- Observation
- Entity
- Person
- Agent
- Organization
- Project
- Decision
- Requirement
- Task
- Incident
- Source
- Episode
- Conversation
- Turn
- Document

### Code

- Repository
- RepositorySnapshot
- Branch
- Commit
- Directory
- File
- FileVersion
- Package
- Module
- Symbol
- SymbolVersion
- Function
- Method
- Class
- Interface
- Type
- Variable
- Constant
- Schema
- Table
- Column
- APIEndpoint
- Test
- Configuration
- Queue
- Event
- ExternalService

### Development lifecycle

- Issue
- PullRequest
- Build
- Deployment
- BenchmarkRun
- AgentAction

## Core relationship families

```text
CONTAINS
DEFINES
REFERENCES
CALLS
IMPORTS
EXPORTS
IMPLEMENTS
EXTENDS
OVERRIDES
INSTANTIATES
READS
WRITES
MUTATES
DEPENDS_ON
TESTS
DOCUMENTS
CONFIGURES
PUBLISHES
CONSUMES
ROUTES_TO
USES_SCHEMA

DERIVED_FROM
SUPPORTED_BY
CONTRADICTS
SUPERSEDES
INVALIDATES
RELATED_TO

CREATED_IN
INTRODUCED_IN
MODIFIED_IN
REMOVED_IN
RENAMED_FROM

REQUESTED_BY
IMPLEMENTS_REQUIREMENT
RESOLVES
DECIDED_IN
DISCUSSED_IN
IMPLEMENTED_BY
DEPLOYED_AS
OBSERVED_BY
```

---

# 5. Target Persistence Model

## 5.1 Canonical IDs

Never use mutable names as identity.

Repository identity must not equal repository path.

Symbol identity must not equal fully qualified name alone.

Recommended pattern:

```text
repository_id
snapshot_id
stable_symbol_id
symbol_version_id
file_id
file_version_id
entity_id
claim_id
claim_version_id
source_id
evidence_id
```

## 5.2 Stable symbol identity

A symbol needs two identities.

### Stable conceptual identity

Represents the logical symbol over its lifetime.

Example:

`UserService.CreateUser`

### Snapshot occurrence

Represents that symbol at a particular Git state.

Example:

`UserService.CreateUser @ commit abc123`

This permits:

- rename tracking;
- move tracking;
- signature changes;
- symbol history;
- historical queries;
- branch divergence.

## 5.3 Temporal claim model

Suggested logical schema:

```text
claims
  id
  tenant_id
  subject_entity_id
  predicate
  object_entity_id
  literal_value
  confidence
  epistemic_class
  valid_from
  valid_to
  recorded_at
  superseded_at
  source_id
  extractor
  extractor_version
  metadata
```

Multiple contradictory claims may coexist if they have different evidence or validity ranges.

Do not destroy earlier assertions simply because a newer one exists.

## 5.4 Evidence model

```text
evidence
  id
  tenant_id
  source_type
  source_id
  source_locator
  content_hash
  snapshot_id
  commit_sha
  created_at
  metadata
```

`source_locator` may contain:

- conversation turn;
- document span;
- file and line range;
- Git diff;
- API response;
- benchmark output.

---

# 6. Code Intelligence Rebuild

This is the highest-priority engineering work.

The existing `LanguageAdapter` abstraction should survive.

The heuristic implementation beneath it should not.

## Target indexer capability tiers

### Tier 1: SCIP / compiler semantic indexes

Preferred wherever mature indexers exist.

Provides:

- definitions;
- references;
- implementations;
- symbol identities;
- types;
- documentation;
- relationships.

### Tier 2: Language Server Protocol

Use where a reliable LSP can provide semantic relationships but SCIP is unavailable.

### Tier 3: Tree-sitter

Universal syntactic fallback.

Provides:

- syntax tree;
- functions;
- classes;
- imports;
- declarations;
- syntactic call sites.

### Tier 4: heuristic extraction

Last resort only.

Must be explicitly labelled as low-confidence.

---

# 7. Code Indexing Pipeline

Every repository index operation should become a durable job.

## Pipeline

```text
REGISTER REPOSITORY
       │
       ▼
RESOLVE GIT SNAPSHOT
       │
       ▼
CREATE IMMUTABLE SNAPSHOT
       │
       ▼
COMPUTE GIT DIFF
       │
       ▼
DETECT RENAMES / MOVES
       │
       ▼
HASH FILE CONTENT
       │
       ▼
SKIP UNCHANGED FILES
       │
       ▼
PARSE CHANGED FILES
       │
       ▼
IMPORT SCIP/LSP DATA
       │
       ▼
TREE-SITTER FALLBACK
       │
       ▼
GENERATE SYMBOL VERSIONS
       │
       ▼
DIFF SYMBOL GRAPH
       │
       ▼
UPDATE RELATION EDGES
       │
       ▼
UPDATE LEXICAL INDEX
       │
       ▼
UPDATE EMBEDDINGS
       │
       ▼
UPDATE GRAPH PROJECTION
       │
       ▼
RUN VALIDATION
       │
       ▼
ATOMically ACTIVATE SNAPSHOT
```

No partially built snapshot may become active.

---

# 8. Snapshot Model

Add:

```text
repository_snapshots
  id
  repository_id
  branch
  commit_sha
  parent_snapshot_id
  extractor_version
  configuration_hash
  status
  created_at
  activated_at
```

Statuses:

```text
queued
scanning
parsing
indexing
validating
ready
active
failed
superseded
```

An active snapshot is immutable.

A new commit produces another snapshot.

---

# 9. Fix Graphify Immediately

The existing Graphify cache must stop using repository-path identity alone.

Current concept:

```text
hash(repo_path)
```

Replace with:

```text
hash(
  repository_id
  + commit_sha
  + graphify_version
  + extraction_config_hash
)
```

Graph artifacts become immutable.

Example:

```text
graph-cache/
  <repository-id>/
    <commit-sha>/
      <extractor-version>/
        graph.json
        manifest.json
```

`manifest.json`:

```json
{
  "repository_id": "...",
  "commit_sha": "...",
  "extractor_version": "...",
  "config_hash": "...",
  "generated_at": "...",
  "graph_hash": "..."
}
```

Never overwrite old snapshots in place.

---

# 10. Durable Code Storage

Retire the whole-state JSON persistence path.

## Production baseline

Use PostgreSQL.

Recommended tables:

```text
repositories
repository_snapshots

files
file_versions

symbols
symbol_versions

code_edges
code_edge_evidence

code_chunks
code_embeddings

indexing_jobs
indexing_job_steps

impact_predictions
impact_reconciliations
```

## Indexes

At minimum:

```text
repository_id
snapshot_id
stable_symbol_id
symbol FQN
file path
commit SHA
symbol kind
edge source
edge target
edge type
content hash
```

Add PostgreSQL:

- FTS;
- `pg_trgm`;
- pgvector or VectorChord-compatible vector storage.

Dedicated search engines should remain optional scale-out adapters rather than mandatory dependencies.

---

# 11. Retrieval Architecture

SCOS retrieval should become federated rather than relying on one similarity score.

A query is first classified into retrieval intents.

Examples:

```text
memory_fact
historical
decision_reason
exact_symbol
code_search
dependency
change_impact
debugging
architecture
cross_memory_code
```

The orchestrator selects appropriate retrievers.

## Retriever set

### Exact identity retrieval

For:

- symbol names;
- paths;
- IDs;
- commit SHAs;
- known entities.

### Lexical retrieval

BM25 / PostgreSQL FTS / trigram.

Essential for identifiers where embeddings can be embarrassingly imaginative.

### Dense retrieval

For conceptual similarity.

### Graph retrieval

For relationships and neighbourhood expansion.

### Temporal retrieval

For:

- what was true;
- what changed;
- what existed at a date;
- supersession.

### Git retrieval

For:

- commits;
- diffs;
- authors;
- blame-style history;
- branch ancestry;
- symbol history.

### Structured metadata retrieval

For:

- language;
- repository;
- service;
- visibility;
- symbol kind;
- tenant;
- confidence.

---

# 12. Retrieval Fusion

Parallel retrieval results should be normalized and fused.

Initial implementation:

**Reciprocal Rank Fusion.**

Do not invent a giant hand-tuned scoring formula on day one.

After enough evaluation data exists, introduce learned ranking.

Candidate features:

```text
exact_identity_match
lexical_score
dense_similarity
graph_distance
relation_strength
repo_locality
branch_match
commit_recency
temporal_fit
source_reliability
confidence
access_frequency
importance
evidence_diversity
historical_agent_success
```

---

# 13. Reranking

After fusion:

1. remove access-ineligible results;
2. deduplicate;
3. collapse overlapping chunks;
4. rerank highest-value candidates;
5. enforce source diversity;
6. construct token-budgeted context.

Cross-encoder reranking can be added after deterministic baselines exist.

Do not use an expensive model to disguise a weak index.

---

# 14. Context Builder

The context builder should receive a task budget:

```text
max_tokens
max_memory_tokens
max_code_tokens
max_history_tokens
max_graph_tokens
```

It should optimize for **marginal information gain per token**.

Each selected item gets a reason:

```text
selected because:
- exact requested symbol
- direct caller
- current implementation
- supporting architectural decision
- relevant recent change
```

The resulting context package should be inspectable in the dashboard.

---

# 15. Memory Evolution

The current fact lifecycle is worth retaining.

Build on it instead of rewriting it merely because competitors use different nouns.

Add four major layers.

## Layer 1: raw episodic evidence

Conversation turns and events.

## Layer 2: semantic claims

Normalized assertions.

Example:

```text
SCOS deployment uses PostgreSQL
```

## Layer 3: observations

Evidence-backed conclusions derived from multiple claims/events.

Example:

```text
The project consistently prefers PostgreSQL-backed canonical storage.
```

## Layer 4: mental/project models

Compact maintained representations for recurring domains.

Examples:

```text
user preferences
project architecture
repository conventions
agent capabilities
deployment topology
```

These are derived and refreshable, not unquestionable truth.

---

# 16. Contradiction Handling

Do not model contradiction purely as deletion or replacement.

Example:

```text
Claim A:
API port = 8080
valid 2026-05-01 → 2026-07-14

Claim B:
API port = 8443
valid 2026-07-14 → present
```

Both are correct.

The system should distinguish:

- temporal replacement;
- actual contradiction;
- uncertain conflict;
- perspective-dependent belief.

---

# 17. Observer-Aware Knowledge

Later, introduce perspective-aware assertions.

Model:

```text
observer
subject
predicate
object
```

This enables:

```text
Agent A believes X
Agent B believes Y
User explicitly stated Z
SCOS canonical evidence currently supports Y
```

This is critical in multi-agent environments because one agent's assumption should not silently become global truth.

---

# 18. Automatic Memory Invalidation From Code

This should become a flagship SCOS capability.

Example chain:

```text
Memory:
"authentication is handled in AuthService.Login"

         ↓ evidence link

Symbol:
AuthService.Login

         ↓ code change

commit deletes symbol

         ↓

SCOS marks memory:
potentially_stale

         ↓

revalidation job

         ↓

new implementation found:
IdentityService.Authenticate

         ↓

old claim valid_to set
new claim created
lineage preserved
```

No major competitor owns this full workflow.

Build toward it deliberately.

---

# 19. Decision-to-Code Provenance

Add explicit nodes:

```text
Decision
Requirement
Task
PullRequest
Commit
SymbolVersion
```

Relations:

```text
DECIDED_IN
CREATES_REQUIREMENT
IMPLEMENTED_BY
RESOLVED_BY
MODIFIED_IN
AFFECTS_SYMBOL
```

Where automation cannot establish the link deterministically, inferred links must retain confidence and evidence.

---

# 20. Change Impact Engine

The existing change-impact/reconciliation concept should evolve rather than be discarded.

Input:

```text
repository
snapshot
changed symbols/files
```

Candidate impact sources:

- reverse dependency graph;
- call graph;
- imports;
- test coverage relations;
- API consumers;
- schema consumers;
- configuration dependencies;
- Git co-change history;
- historical impact reconciliations.

Output:

```text
affected entity
impact type
confidence
evidence
reason
recommended validation
```

Then compare predictions against what actually happened.

Use the reconciliation history as training/evaluation data.

This creates a possible future learning loop:

```text
prediction
→ real outcome
→ reconciliation
→ scoring adjustment
→ better future impact prediction
```

---

# 21. Storage Strategy

## Local / embedded deployment

Target:

- SQLite or libSQL canonical subset;
- FTS5;
- embedded vector index;
- local object/artifact directory.

Purpose:

- laptop development;
- offline agent use;
- single-user environments.

## Production default

Target:

- PostgreSQL 16+;
- FTS;
- pg_trgm;
- pgvector or VectorChord-compatible vectors;
- object storage;
- optional Redis/Valkey for ephemeral caching.

## Scale-out options

Only introduce when benchmarks justify them:

- Zoekt/OpenSearch for huge lexical corpora;
- dedicated vector stores for extreme embedding scale;
- Neo4j/Memgraph/FalkorDB when relational graph execution becomes the measured bottleneck;
- Kafka/NATS/Redpanda class systems when PostgreSQL-backed durable jobs stop being sufficient.

Do not begin by deploying seven databases because somebody made an architecture diagram with seven logos.

---

# 22. Durable Job System

Indexing, embedding, consolidation, graph building and revalidation need durable jobs.

Every job:

```text
id
tenant
type
input identity
generation
status
attempt
checkpoint
lease owner
lease expiry
started
heartbeat
completed
error
```

Required properties:

- idempotent;
- resumable;
- observable;
- retryable;
- cancellable;
- tenant-safe.

Workers must never report success when output validation failed.

---

# 23. Versioning Everywhere

Every running service should expose:

```json
{
  "source_sha": "...",
  "build_id": "...",
  "schema_version": "...",
  "index_format_version": "...",
  "config_fingerprint": "...",
  "built_at": "..."
}
```

The deployment control plane should refuse to declare a release healthy when expected and observed versions disagree.

This directly prevents another repo-versus-production drift class of failure.

---

# 24. Deletion and Privacy Architecture

Deletion must propagate to every derived structure.

A deletion ledger should record:

```text
canonical record
lexical projection
vector projection
graph projection
cache
backup retention state
derived observation
derived mental model
```

Deletion success means every required projection has either:

- deleted the material;
- tombstoned it;
- or recorded a compliant retention exemption.

A row disappearing from one PostgreSQL table is not proof of deletion.

---

# 25. Security Invariants

Every query and indexing operation must be tenant scoped before retrieval.

Never:

```text
search globally
→ rerank
→ filter tenant
```

Always:

```text
authorize
→ establish tenant scope
→ retrieve inside scope
→ rank
```

Cache keys must include authorization boundaries.

Internal services need authenticated identities.

Client-supplied tenant headers must not be trusted as authorization.

Credentials exposed to agents must be scoped, auditable and revocable.

---

# 26. Observability

Every retrieval request should create a trace containing:

```text
query
classified intent
retrievers invoked
retriever latency
candidate counts
fusion ranking
reranker scores
selected context
token count
source/evidence IDs
agent/task result
```

Every indexing run should expose:

```text
files examined
files skipped
files changed
symbols created
symbols updated
symbols removed
edges changed
embeddings changed
duration
errors
snapshot activated
```

Without this, performance tuning becomes folklore.

---

# 27. Benchmark System

Benchmarks must become release gates.

## Memory benchmark categories

- factual recall;
- temporal reasoning;
- contradictions;
- updates/supersession;
- long-horizon conversations;
- entity resolution;
- deletion;
- privacy isolation;
- provenance correctness.

Public suites where reproducible:

- LongMemEval;
- LoCoMo;
- BEAM-style evaluation where applicable.

Also build SCOS-native suites competitors cannot cover.

## Code benchmarks

Measure:

- definition resolution;
- reference resolution;
- symbol search;
- repository search;
- call/dependency accuracy;
- impact prediction;
- rename lineage;
- historical queries;
- incremental indexing correctness.

Use compiler/LSP truth where possible.

## Agent-level benchmarks

Compare:

```text
Agent alone

Agent + memory only

Agent + code intelligence only

Agent + unified SCOS
```

Measure:

- task success;
- wrong-file edits;
- hallucinated symbols;
- unnecessary tool calls;
- tokens;
- wall-clock latency;
- regressions;
- reverted edits.

---

# 28. Mandatory Retrieval Ablations

Every major benchmark should compare:

```text
dense only
lexical only
exact symbols only
graph only
temporal only

lexical + dense
lexical + dense + graph
hybrid + temporal
hybrid + reranker
full unified retrieval
```

Otherwise SCOS will not know which expensive components actually improve anything.

---

# 29. Initial Performance Targets

These are engineering targets, not marketing claims.

## Memory

Durable explicit write:

`p95 < 100 ms`, excluding model extraction.

Extracted memory availability:

`p95 < 3 seconds`.

Simple retrieval:

`p95 < 150 ms` before reranking.

Hybrid retrieval:

`p95 < 500 ms` including reranking under normal loads.

## Code

Exact symbol lookup:

`p95 < 30 ms`.

Lexical code search:

`p95 < 100 ms`.

One-to-two-hop graph traversal:

`p95 < 100 ms`.

Small changed-file incremental update:

`< 2 seconds` target.

New branch snapshot visible:

`< 10 seconds` target for ordinary changes.

## Correctness

Serving a known-stale active index:

`0 accepted cases`.

---

# 30. Repository Structure Target

Do not immediately split everything into more microservices.

Prefer reusable packages beneath existing service boundaries.

Suggested additions:

```text
packages/
  evidence/
  temporal/
  retrieval/
  ranking/
  codegraph/
  codeindex/
  snapshots/
  provenance/
  jobs/
  benchmark/

services/
  memory-intelligence/
  code-intelligence/
  context-optimisation/
  graphify-provider/
```

Inside code intelligence:

```text
services/code-intelligence/
  cmd/server/

  internal/
    repository/
    snapshots/
    indexing/
    scip/
    treesitter/
    symbols/
    relations/
    lexical/
    embedding/
    impact/
    query/
```

Do not create a separate service for each of those.

---

# 31. Phase 0: Establish Ground Truth

## Objective

Stop architectural work from proceeding without measurable baselines.

## Modification

Create a benchmark and correctness harness before the main rewrite.

## Procedure

Implement:

```text
packages/benchmark/
benchmarks/memory/
benchmarks/code/
benchmarks/retrieval/
benchmarks/agent/
testdata/repos/
```

Capture the current system's performance before replacing it.

Create fixtures for:

- exact symbol resolution;
- renamed symbol;
- deleted file;
- branch divergence;
- stale Graphify cache;
- memory supersession;
- temporal contradiction;
- tenant isolation.

## Benchmark

Every benchmark produces machine-readable JSON.

Persist:

- source SHA;
- schema version;
- index version;
- configuration;
- model;
- hardware where relevant.

## Diagnostic threshold

No subsequent phase may regress critical correctness benchmarks.

---

# 32. Phase 1: Kill Stale Graph State

## Objective

Eliminate known incorrect graph caching.

## Modification

Make Graphify cache immutable and snapshot addressed.

## Procedure

Introduce:

```text
repository_id
commit_sha
extractor_version
config_hash
```

into graph identity.

Create graph manifests.

Reject graph results when manifest snapshot differs from requested repository snapshot.

## Benchmark

Change a repository after graph creation.

Query for removed or modified code.

Old graph must never be served as current.

## Completion gate

`0` stale-current responses across the regression suite.

---

# 33. Phase 2: Durable Repository Snapshots

## Objective

Replace mutable in-memory repository state with durable immutable snapshots.

## Modification

Create PostgreSQL schema for:

```text
repositories
repository_snapshots
files
file_versions
symbols
symbol_versions
code_edges
```

## Procedure

Keep existing public API compatibility while replacing internal store implementation.

Introduce a storage interface:

```go
type CodeStore interface {
    CreateSnapshot(...)
    GetSnapshot(...)
    ActivateSnapshot(...)
    UpsertFileVersion(...)
    UpsertSymbolVersion(...)
    PutEdges(...)
    QuerySymbols(...)
}
```

Provide PostgreSQL implementation.

Keep JSON import support temporarily for migration only.

Do not keep JSON as the runtime store.

## Completion gate

Restart the code-intelligence service and obtain identical query results from persistent storage.

Run two service instances against the same database without inconsistent local state.

---

# 34. Phase 3: Semantic Code Extraction

## Objective

Replace filename-level approximation with real source-code intelligence.

## Modification

Extend `LanguageAdapter` to report capabilities.

Example:

```go
type Capabilities struct {
    Syntax          bool
    Definitions     bool
    References      bool
    Implementations bool
    Types           bool
    CallGraph       bool
    DataFlow        bool
}
```

## Procedure

Implement ingestion in this preference order:

```text
SCIP
↓
language-native semantic index
↓
LSP
↓
Tree-sitter
↓
heuristic fallback
```

Initially target the languages most important to SCOS itself:

- Go;
- Python;
- TypeScript/JavaScript.

Then expand.

## Completion gate

Definition/reference benchmark accuracy is measured against compiler/LSP truth.

Heuristic extraction cannot be reported as “precise.”

---

# 35. Phase 4: Incremental Git-Aware Indexing

## Objective

Index changes, not repositories.

## Procedure

For every new snapshot:

1. find parent;
2. calculate Git diff;
3. detect renames;
4. hash changed files;
5. skip unchanged files;
6. parse changed files;
7. calculate symbol fingerprints;
8. create/update/remove symbol versions;
9. rebuild affected relationships;
10. update lexical projections;
11. update changed embeddings;
12. validate;
13. activate.

Create tombstones for removed entities.

Maintain rename/move lineage.

## Completion gate

A one-line change in one file must not trigger a full repository re-index.

Unchanged symbol IDs remain stable across snapshots.

---

# 36. Phase 5: Hybrid Retrieval Foundation

## Objective

Make retrieval competitive before adding more “intelligence.”

## Modification

Introduce a retrieval interface:

```go
type Retriever interface {
    Search(ctx context.Context, query Query) ([]Candidate, error)
}
```

Implement:

```text
ExactRetriever
LexicalRetriever
DenseRetriever
GraphRetriever
TemporalRetriever
GitRetriever
```

Add RRF fusion.

## Completion gate

Ablation tests demonstrate that hybrid retrieval beats the strongest individual retriever on the target evaluation suite.

---

# 37. Phase 6: Temporal Evidence Model

## Objective

Move from simple supersession to historically queryable truth.

## Modification

Introduce:

```text
valid_from
valid_to
recorded_at
superseded_at
```

for claims and mutable relationships.

## Procedure

Migrate existing fact lineage without destroying historical records.

Represent contradiction separately from supersession.

## Completion gate

SCOS can answer:

```text
What is true now?
What was true on date X?
What did SCOS believe on date X?
When did the world change?
When did SCOS learn it changed?
```

with different correct answers where appropriate.

---

# 38. Phase 7: Unified Memory + Code Graph

## Objective

Make memory and code intelligence one queryable evidence space.

## Modification

Create generic entity and relationship identities while retaining specialized code tables for efficient indexing.

## Procedure

Start linking:

```text
conversation → decision
decision → requirement
requirement → task
task → PR
PR → commit
commit → symbol version
symbol → test
symbol → service/API/schema
```

## Completion gate

A query asking “why does this code exist?” can traverse from current code to at least one original project decision where evidence exists.

---

# 39. Phase 8: Automatic Staleness and Revalidation

## Objective

Make old knowledge respond to code changes.

## Procedure

When a snapshot changes:

1. identify claims backed by changed/deleted code;
2. flag affected claims;
3. attempt deterministic remapping;
4. schedule semantic revalidation where necessary;
5. create successor claims;
6. preserve prior valid-time history.

Statuses:

```text
current
potentially_stale
stale
contradicted
superseded
revalidated
```

## Completion gate

Deleting or renaming a referenced symbol automatically changes the status of dependent knowledge.

---

# 40. Phase 9: Impact Intelligence

## Objective

Turn static relationships into predictive engineering intelligence.

## Procedure

Combine:

- reverse dependencies;
- call graph;
- schemas;
- test relationships;
- historical co-change;
- previous impact reconciliations.

Track predictions and actual outcomes.

## Completion gate

Impact precision and recall are quantitatively measured.

Do not call the engine “AI impact analysis” until it beats dependency-only baselines.

---

# 41. Phase 10: Agent-Outcome Learning

## Objective

Use real agent results to improve retrieval.

Every agent task should optionally record:

```text
retrieved context
used context
edits
tests
success/failure
reverts
human corrections
```

Use this initially for offline ranking evaluation.

Do not immediately let the system self-modify production ranking from noisy outcomes.

First:

```text
collect
→ evaluate
→ train
→ shadow-test
→ canary
→ promote
```

---

# 42. Phase 11: Observer-Aware Multi-Agent Memory

## Objective

Prevent multiple agents' interpretations from collapsing into one fake global truth.

Add observer-relative representations.

Support:

```text
agent → user
agent → project
agent → agent
user → agent
canonical evidence → subject
```

This is particularly valuable once SCOS orchestrates many autonomous agents.

---

# 43. Phase 12: Embedded Local Mode

## Objective

Make SCOS usable without running the entire hosted platform.

Provide:

```text
scos init
scos repo add .
scos status
scos query ...
```

Embedded mode should provide:

- local memory;
- repository index;
- local search;
- MCP;
- background sync when configured.

The hosted platform becomes a scale and synchronization option, not a prerequisite for useful memory.

---

# 44. Phase 13: Deeper Program Analysis

Only after retrieval and semantic indexing benchmarks justify it.

Add selective Code Property Graph capabilities:

- control-flow;
- data-flow;
- taint/data dependencies;
- interprocedural analysis.

Prefer integrating mature tooling such as CPG/Joern-style representations rather than recreating compiler research from scratch.

---

# 45. What Not to Build Yet

Do **not** prioritize:

- another dashboard redesign;
- another model provider;
- another named microservice;
- a dedicated graph database;
- a custom vector database;
- custom compiler frontends for every language;
- reinforcement-learning retrieval;
- fully autonomous ontology generation;
- elaborate multi-agent cognition.

Those can become useful later.

Right now they would mostly increase surface area around the unresolved core.

---

# 46. Immediate Engineering Queue

These should be the next implementation tickets, in order.

## P0-001 Benchmark Baseline

Create machine-readable baseline tests for memory, code and retrieval.

## P0-002 Graphify Snapshot Cache

Replace path-only cache identity with repository snapshot identity.

## P0-003 Repository Snapshot Schema

Add migrations and repository snapshot model.

## P0-004 Durable Code Store

Replace in-memory maps/whole-state JSON runtime persistence with PostgreSQL.

## P0-005 Code Store Migration

Provide one-time import from existing state.

## P0-006 SCIP Ingestion Interface

Create semantic index importer and capability model.

## P0-007 Tree-sitter Fallback

Implement syntax extraction for Go, Python and TypeScript/JavaScript.

## P0-008 Incremental Index Planner

Git diff, hashes, tombstones and rename tracking.

## P0-009 Lexical Code Index

FTS/trigram repository search.

## P0-010 Exact Symbol Index

Fast FQN/stable-ID/path-based lookup.

## P0-011 Retrieval Orchestrator

Parallel retriever execution plus RRF.

## P0-012 Retrieval Trace

Explain candidate source, score and selection.

## P0-013 Temporal Claim Migration

Add valid/transaction time without destroying existing supersession.

## P0-014 Canonical Evidence IDs

Unify provenance identifiers across memory and code.

## P0-015 Release Manifest

Enforce source SHA/config/schema/index parity in deployment.

---

# 47. First Milestone

The first meaningful milestone is **not** “all competitor features implemented.”

It is:

> SCOS can index its own repository precisely and incrementally, remember the project history, retrieve memory and code through one orchestrator, and prove through benchmarks that the resulting context is better than memory-only or code-only retrieval.

Acceptance scenario:

1. index SCOS at commit A;
2. ask where a capability lives;
3. return exact symbols and relevant project memory;
4. change the implementation at commit B;
5. incrementally index only affected portions;
6. preserve historical snapshot A;
7. identify changed symbol lineage;
8. invalidate stale memory referencing old implementation;
9. answer what exists now;
10. answer what existed at A;
11. explain why the design was introduced;
12. provide evidence for all three answers.

If SCOS can do that reliably, the architectural thesis is proven.

---

# 48. Second Milestone

SCOS handles multiple repositories and multiple agents while preserving:

- tenant isolation;
- observer-specific beliefs;
- Git history;
- decision provenance;
- cross-repository dependencies;
- retrieval quality;
- deterministic deletion.

The system should answer:

> If I change this schema, which repositories, services, tests and historical design assumptions are affected?

with evidence.

---

# 49. Third Milestone

SCOS begins improving agent outcomes measurably.

Release criteria should include statistically meaningful improvement in:

- task success;
- code-edit correctness;
- retrieval precision;
- hallucinated-symbol reduction;
- unnecessary tool calls;
- tokens consumed;
- regressions;
- recovery from stale knowledge.

At this point SCOS becomes more than infrastructure.

It becomes an intelligence multiplier.

---

# 50. Definition of Done

SCOS is approaching “best on the market” when all of the following are true:

### Memory

SCOS matches or exceeds leading memory systems on factual, temporal and contradiction benchmarks.

### Code

SCOS provides compiler/LSP-level precise code navigation for supported languages.

### Retrieval

Hybrid retrieval demonstrably beats individual retrieval methods.

### Temporal

Every mutable assertion can be reconstructed historically.

### Provenance

Important responses can identify evidence.

### Incremental indexing

Small changes produce small indexing workloads.

### Scale

Large repositories do not require loading the corpus into one process.

### Reliability

Stale index generations cannot silently become current.

### Multi-agent

Knowledge can distinguish evidence, canonical conclusions and observer beliefs.

### Security

Tenant isolation and deletion are validated through adversarial tests.

### Agent impact

Agents perform materially better with SCOS than without it.

### Unique capability

SCOS can trace:

```text
conversation
→ decision
→ requirement
→ implementation
→ commit
→ symbol
→ current code
→ later change
→ memory revalidation
```

That final chain should be treated as the strategic north star.

---

# 51. Final Build Order

The build sequence should be:

```text
1. Benchmark current system
2. Fix stale Graphify identity
3. Introduce immutable repository snapshots
4. Replace code JSON/map persistence
5. Add SCIP semantic ingestion
6. Add Tree-sitter fallback
7. Implement incremental Git indexing
8. Build canonical code relationship graph
9. Add exact + lexical retrieval
10. Normalize dense retrieval
11. Add graph + temporal retrieval
12. Add RRF fusion
13. Add retrieval traces
14. Introduce full temporal claim semantics
15. Link memory evidence to code evidence
16. Build decision → task → commit → symbol lineage
17. Implement code-driven memory staleness
18. Improve change-impact prediction
19. Collect agent outcome signals
20. Tune retrieval from measured outcomes
21. Add observer-relative multi-agent knowledge
22. Ship embedded/local-first mode
23. Add deeper CPG/data-flow analysis where benchmarks justify it
24. Publish reproducible benchmark results
```

---

# 52. Immediate Rule for the Repository

Until steps 1 through 12 are substantially complete:

> **Do not expand SCOS horizontally unless the new feature directly improves benchmarked memory quality, code intelligence, retrieval, provenance, security, or reliability.**

SCOS already has enough surface area.

The next stage is not about making it larger.

It is about turning the pieces that already exist into a coherent, measurable intelligence system.

The highest-leverage next commit after this blueprint should therefore start the **benchmark baseline and immutable repository-snapshot foundation**, followed immediately by removal of the JSON/map code-intelligence runtime store.

That sequence unlocks nearly everything else in the roadmap.
