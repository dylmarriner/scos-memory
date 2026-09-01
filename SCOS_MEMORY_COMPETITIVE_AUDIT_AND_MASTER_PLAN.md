# SCOS Memory Competitive Audit and Master Plan

**Repository audited:** `dylmarriner/scos-memory`  
**Pinned SCOS revision:** `78755c40b2ba36300313da1ba5c5df029ff4aab6` (`master`, 2026-07-22)  
**Audit date:** 2026-09-01  
**Scope:** persistent agent memory + repository/code intelligence + temporal/code graph + retrieval + production platform  
**Evidence classes:** **Verified** = inspected implementation/source; **Documented** = project docs/README state it but implementation was not fully traced; **Inferred** = architecture implies it; **Unknown** = insufficient evidence.

> **Audit limitation.** SCOS was inspected against a pinned GitHub revision through repository APIs because the execution environment could not clone GitHub over outbound DNS. Core implementations were read directly, including memory-intelligence, code-intelligence, graphify-provider, deployment history, repository/package structure, and supporting files. Competitors were inspected from current public GitHub source/docs available on 2026-09-01. This is a forensic architecture audit, not a substitute for running every competitor's private/hosted backend under identical load. Hosted-only claims are explicitly marked as such.

---

## 1. Executive Summary

SCOS Memory is not merely a memory SDK. It is already shaped like a **distributed agent knowledge platform**: Go services, tenant-aware APIs, memory lifecycle logic, code-intelligence services, graphification, context optimisation, agent integrations, observability, reliability components, cloud/control-plane pieces, and adapter packages. That breadth is its strongest legitimate differentiator.

The problem is that **product breadth is ahead of algorithmic depth in several critical planes**.

The memory plane has real substance. The inspected `memory-intelligence` implementation contains normalized fact keys/values, scope, confidence, importance, canonical value, encryption flags, source session/turn provenance, TTL, forgotten state, explicit supersession (`supersedes_id` / `superseded_by_id`), advisory locking, duplicate review, merge logging, and bucketed access-frequency tracking. That is not a README fantasy. It is implemented.

The code-intelligence plane is much weaker. At the pinned revision, `services/code-intelligence/cmd/server/main.go` keeps repositories, symbols, chunks, generations, relations and reconciliation records in Go maps and optionally serializes the entire state to one JSON file. Symbol extraction is driven by language/filename conventions rather than a real parser/language-server/SCIP pipeline. Querying includes exact/suffix/substring-style logic rather than a production lexical+dense+precise-symbol+graph retrieval fabric. That architecture cannot credibly support large monorepos, cross-repository references, tens of millions of symbols, high-concurrency indexing, or precise impact analysis.

SCOS also integrates an external Graphify CLI through `services/graphify-provider`. That integration is operationally thoughtful: arguments are not shell-expanded, flag-like argument injection is rejected, graph cache keys are hashed, graph output is parsed with explicit confidence metadata, and worker count/clustering were bounded after observed OOM behavior. But the service itself explicitly states that cached graph existence is checked **without staleness invalidation**. A changed repository can therefore leave stale graph state until manual cache deletion/force rebuilding. For a system claiming live code intelligence, that is a P0 correctness defect, not a minor cache detail.

### The competitive verdict

SCOS is currently:

- **Stronger than typical memory libraries in platform breadth**, tenant-aware service decomposition, agent/tool integration ambition, and the opportunity to unify memory with code history.
- **Competitive in basic durable fact lifecycle mechanics**, especially explicit supersession, forgetting, access tracking and scope-aware merging.
- **Behind Hindsight/Graphiti in memory retrieval sophistication and temporal graph semantics.**
- **Behind Honcho in peer/social cognition and observer→observed representations.**
- **Behind Letta in agent-controlled working/core-memory ergonomics.**
- **Behind Cognee in pluggable multi-store knowledge infrastructure and pipeline extensibility.**
- **Far behind Sourcegraph/SCIP in precise compiler/language-server-derived code intelligence.**
- **Far behind Joern in code-property-graph depth (AST + CFG + data/control-flow semantics).**
- **Behind Aider's repo-map technique in graph-centrality-based context selection, despite Aider being much smaller in product scope.**
- **Missing a defensible public benchmark story** proving memory accuracy, graph correctness, code navigation accuracy, impact-analysis precision, latency, scale, and end-to-end agent improvement.

### The most important strategic decision

Do **not** turn SCOS into a pile of separate databases and fashionable algorithms simply because competitors use them. The winning design is a **unified temporal evidence graph** with specialized indexes layered over one canonical evidence model.

The target should be:

> **One evidence model, multiple retrieval indexes, explicit time, immutable provenance, deterministic code facts when possible, probabilistic semantic facts when necessary, and every assembled context item traceable back to evidence.**

### Seven P0 changes

1. Replace the JSON-file code-intelligence store with a durable indexed persistence layer and immutable snapshot/generation model.
2. Replace filename/convention symbol extraction with real parser/indexer adapters: Tree-sitter for broad syntax coverage plus SCIP/LSP/compiler-native indexers for semantic precision.
3. Introduce a canonical graph schema with typed nodes/edges, confidence, extractor version, provenance and bitemporal validity.
4. Build true incremental/Git-aware indexing: content/AST/symbol hashes, diff/rename detection, branch snapshots, tombstones, dependency invalidation and resumable jobs.
5. Replace ad-hoc code query matching with a federated retrieval orchestrator: exact symbol + lexical + dense + graph + temporal, fused and reranked.
6. Make graph and search cache invalidation generation-aware so stale graph/index results are impossible to serve silently.
7. Establish reproducible benchmarks before adding more named services.

### The moat

SCOS should not try to become “Mem0 plus Graphiti plus Sourcegraph.” That would create a large, expensive imitation.

The defensible product is **commit-aware autobiographical intelligence for agents**:

- a user decision can point to the conversation where it was made;
- that decision can point to a requirement/task;
- the task can point to commits and pull requests;
- commits point to changed symbols;
- symbols point to callers, tests, schemas, routes and runtime components;
- later code changes can automatically invalidate or downgrade memories whose factual basis is no longer true;
- retrieval can answer both “what does the code do?” and “why did we make it this way?” with evidence.

Very few systems span that entire chain. SCOS already has the right product boundary to attempt it. It does not yet have the indexing and temporal foundations to deliver it.

---

## 2. Current scos-memory Architecture

### 2.1 Repository shape

At the pinned revision the repository is primarily Go and organized around service boundaries. The inspected tree contains root-level `adapters/`, `cmd/`, `deploy/`, `docs/`, `packages/`, `scripts/`, and `services/`, with Docker/Compose deployment assets and GitHub automation.

Observed services include, among others:

- `agent-registry`
- `autonomous-control-plane`
- `backup-manager`
- `cloud-control-plane`
- `code-intelligence`
- `compatibility-gateway`
- `context-optimisation`
- `credential-vault`
- `execution-proxy`
- `extension-manager`
- `graphify-provider`
- `hermes-provider`
- `memory-intelligence`
- additional gateway, tenant, temporal, reliability and observability services present in the repository history/tree.

Observed reusable packages include database pooling/TLS, authentication, telemetry, benchmark harnesses, context contracts, adapter SDKs, embeddings, event/citation helpers and symbol-lineage/concurrency utilities.

**Assessment:** the service decomposition anticipates a much larger platform than the current algorithms require. This is simultaneously useful modularity and a tax. Until throughput or security boundaries require independent scaling, overly fine-grained services increase deployment drift, network failure modes, schema/version coordination and observability burden.

### 2.2 Memory-intelligence execution path

**Evidence: Verified.** `services/memory-intelligence/cmd/server/main.go` and `store.go`.

Core server configuration includes:

- `SCOS_MEMORY_INTENT_THRESHOLD` default 0.65
- `SCOS_MEMORY_RETRIEVAL_QUERY_THRESHOLD` default 0.55
- `SCOS_MEMORY_MERGE_DUPLICATE_THRESHOLD` default 0.82
- `SCOS_MEMORY_MERGE_SUPERSEDE_THRESHOLD` default 0.73
- `SCOS_MEMORY_RETRIEVAL_HALF_LIFE_DAYS` default 30
- `SCOS_MEMORY_RETRIEVAL_RECENCY_WEIGHT` default 0.25
- `SCOS_MEMORY_RETRIEVAL_ACCESS_WEIGHT` default 0.08

The turn path accepts tenant identity plus `user_id`, `session_id`, `text` and time. It detects candidate facts, handles explicit forget intent, creates/supersedes facts, detects retrieval-query intent and assembles selected memory.

The stored fact model contains:

- identity: fact ID, tenant, user
- normalized identity: `fact_key_norm`, `fact_value_norm`
- content/type: key, value, type, canonical value
- policy: scope, importance, confidence, encrypted
- evidence: source session, source turn
- lineage: supersedes / superseded-by
- lifecycle: promoted, TTL, created, updated, forgotten

`createOrSupersedeFact` uses a PostgreSQL transaction, a per `(tenant,user,key)` advisory lock, active-row lookup under `FOR UPDATE`, normalized equality checks, and explicit supersession links. This is a sound basis for versioned facts.

`forgetFact` marks forgotten time and deletes associated access rows. `markFactMerged` supersedes a source into a target and appends `memory_fact_merge_log` metadata. `recordAccessBatch` tracks usage in time buckets to avoid unbounded per-read increments.

The duplicate-review path calculates normalized value similarity, observes merge/supersede thresholds, enforces scope boundaries and chooses a survivor based on richness/supersession heuristics.

### 2.3 Current memory strengths

**Verified:**

- tenant/user scoping
- normalized fact keys and values
- explicit supersession lineage
- TTL and forgetting fields
- source session/turn provenance
- confidence and importance fields
- duplicate/merge path with scope guard
- access counts / last-access signal
- transaction + advisory lock for write races
- merge log
- recency/access-aware retrieval parameters

**Not verified / incomplete compared with target:**

- proper event-time vs ingestion-time bitemporality
- contradiction sets with mutually exclusive claims
- LLM or rule-based semantic contradiction resolution with evidence
- observation abstraction backed by multiple evidence items
- hierarchical memories / mental models
- procedural-memory extraction and validation
- explicit entity resolution graph in the canonical memory store
- hybrid BM25+dense+graph retrieval with calibrated fusion
- cross-encoder reranking
- learned retrieval from downstream outcomes
- privacy deletion proofs across all derived indexes

### 2.4 Code-intelligence execution path

**Evidence: Verified.** `services/code-intelligence/cmd/server/main.go`.

The service exposes types for:

- repository
- language adapter
- symbol
- index generation
- chunk
- code query/result
- change impact
- impact reconciliation

The `Symbol` model includes FQN, kind, signature, path/range, parent, language, content hash, commit ID, docs, imports, outbound relations, inbound relation summary, tests and visibility. This is a useful API shape.

However, the underlying `codeStore` is:

```text
map[repo_id]*Repository
map[symbol_id]*Symbol
map[symbol_id]*Chunk
map[repo_id]*IndexGeneration
map[symbol_id][]dependant_symbol_id
[]ImpactReconciliation
```

It is optionally persisted by serializing the complete store to a versioned JSON document, writing a temporary file, `fsync`ing, atomically renaming and syncing the directory. The durability pattern is careful; the data structure is not scalable.

#### Verified consequences

- Every query traversing these maps is bounded by in-process memory.
- There is no durable secondary index for lexical, vector, symbol, commit, path or relation lookup.
- Multi-process horizontal scale is not naturally consistent.
- Large state rewrites become proportional to the whole persisted corpus.
- Crash safety is better than naive file writes, but concurrency and distributed ownership remain local-process concerns.

### 2.5 Symbol extraction

**Evidence: Verified.** The code calls `extractSymbols(file, lang, tier, repoID)`, then language-specific functions such as `extractGoSymbols`, `extractPythonSymbols` and `extractTSJSSymbols`. The implementation constructs package/module-level symbols from file/path conventions.

This is **not equivalent to parsing source code**. It cannot provide compiler-grade definitions/references, overload resolution, type-aware method dispatch, interface implementation, imports with alias resolution, inferred types, local symbols, closures, control flow or data flow.

The current `LanguageAdapter` abstraction is worth preserving, but its implementation target should change from “tiered convention extractor” to “capability-negotiated semantic indexer.”

### 2.6 Graphify-provider

**Evidence: Verified.** `services/graphify-provider/cmd/server/graphify.go`.

The provider invokes an external `graphify` CLI directly through `exec.CommandContext`, never through a shell. It rejects flag-like user positional values to prevent argument injection. It stores graph output under a cache directory keyed by a hash of the canonical repo path.

The implementation comments document observed operational constraints:

- default worker concurrency could OOM the service on large repos;
- clustering could OOM after AST extraction;
- service therefore uses `--max-workers 1` and `--no-cluster` for bounded operation.

It parses node-link JSON with graph nodes and graph edges containing relation, confidence/confidence score, source file and source location.

Most importantly, `ensureCachedGraph` explicitly checks only graph existence, **not source staleness**. A repository changed after graph generation requires cache deletion or force behavior to refresh.

**Severity: Critical for correctness.** A code graph that silently refers to old code is worse than no graph because it produces authoritative-looking wrong answers.

### 2.7 Deployment/repository parity

**Evidence: Verified from repository commit history.** Recent commits include fixes aligning production fabric configuration with repository state after production had drifted several feature batches behind. This reveals a real class of deployment parity issue.

Target requirement: every service must expose build SHA/config fingerprint/schema generation, and deployment canaries must assert that the serving fabric matches the expected release manifest before traffic is considered healthy.

---

## 3. Current Capability Inventory

| Capability | Status | Evidence / Notes |
|---|---|---|
| Durable fact creation | Strong | PostgreSQL-backed transactional fact store |
| Fact version lineage | Strong | explicit supersedes/superseded-by |
| Forgetting | Partial | logical forgetting + access deletion; derived-index cascade not proven |
| TTL | Partial | fact TTL filtering exists; policy sophistication limited |
| Scope isolation | Strong | scope-aware merge guard + tenant/user context |
| Duplicate handling | Partial | normalized similarity + heuristic thresholds |
| Contradiction resolution | Basic | supersession exists; semantic contradiction model not verified |
| Access reinforcement | Partial | bucketed access counts exist |
| Recency weighting | Partial | configurable half-life/weight exists |
| Episodic memory | Basic | source session/turn provenance, but no rich episode model verified |
| Semantic memory | Partial | fact abstraction exists |
| Procedural memory | Missing/Unknown | no verified procedure/rule lifecycle |
| Entity memory | Missing/Partial | not verified as canonical entity graph |
| Bi-temporal memory | Missing | no verified valid-time + transaction-time model |
| Source provenance | Partial | session/turn exists, broader source model needed |
| Vector retrieval | Partial/Unknown | embeddings package exists; full competitive retrieval path not verified |
| Sparse/BM25 retrieval | Missing/Unknown | no verified BM25 index in inspected core |
| RRF/fusion | Missing/Unknown | not verified |
| Cross-encoder reranking | Missing | not verified |
| Graph retrieval | Partial | external Graphify integration |
| Token-budget assembly | Partial | context service/packages exist; deeper quality eval needed |
| Repository registration | Partial | code-intelligence repository model/API |
| Language detection | Partial | language adapters exist |
| AST extraction | Weak | core code-intelligence is convention-based; Graphify provides external AST graph path |
| Precise definitions/references | Missing | no SCIP/compiler/LSP semantic index verified |
| Symbol graph | Basic | relation map exists, semantics limited |
| Call graph | Basic/Unknown | no precise interprocedural call resolution verified |
| CFG | Missing | no verified CFG |
| Data-flow graph | Missing | no verified data-flow/PDG |
| Test→code relation | Partial | model field exists; extraction precision not verified |
| Change impact | Partial | endpoint/model exists, but source graph precision limits correctness |
| Impact outcome reconciliation | Partial/Promising | reconciliation record exists |
| Commit-aware symbol state | Partial | commit ID carried in models |
| Branch-aware snapshots | Basic | repository has branch field; durable branch overlay model absent |
| Incremental changed-symbol indexing | Missing/Weak | no verified semantic hash invalidation pipeline |
| Rename detection | Missing | not verified |
| Resumable indexing jobs | Missing/Unknown | generation model exists; durable queue/checkpoints not verified in code core |
| Cache invalidation | Weak | Graphify cache explicitly lacks staleness detection |
| MCP/agent integrations | Partial/Strong direction | gateway/provider architecture exists |
| Multi-tenancy | Partial/Strong direction | tenant filtering exists; all services need uniform enforcement tests |
| Observability | Partial | telemetry packages/services; end-to-end SLO evidence incomplete |
| Benchmarking | Basic | benchmark harness package exists; market-comparable suite not established |
| Local-first | Partial | service/container orientation; lightweight embedded code store exists but is not sufficient |
| Enterprise HA | Missing/Partial | service decomposition exists; DR, queues, migration and consistency proof incomplete |

---

## 4. Competitive Landscape

### 4.1 Comparison set

| Project | Public source inspected | License / status | Primary strength | Maturity assessment |
|---|---|---|---|---|
| Hindsight (Vectorize) | `vectorize-io/hindsight` main/docs, 2026-09-01 | MIT documented | multi-strategy memory retrieval, observations, mental models, production tooling | High and rapidly active |
| Honcho | `plastic-labs/honcho` main, docs/changelog | Open-source repo; verify exact license in release due diligence | peer/social-cognition memory, background conclusions/representations | High |
| Mem0 | `mem0ai/mem0` main | Apache-style/open-source project; hosted optimizations partly proprietary | broad integrations, extraction/storage adapters, strong managed benchmarks | High |
| Letta / MemGPT | `letta-ai/letta`, `letta-ai/letta-code` | Open source | agent-controlled core memory + external memory + stateful runtime | High |
| Graphiti | `getzep/graphiti` main | Open source | bi-temporal KG, incremental episodes, graph+semantic+full-text retrieval | High |
| Zep | `getzep/zep` | current CE deprecated; Graphiti open-source, Zep cloud current | hosted temporal context assembly / enterprise | High hosted; legacy OSS server deprecated |
| Cognee | `topoteretes/cognee` | Apache-2.0 | pluggable relational/vector/graph stores, pipelines, ontology, graph memory | High |
| LangMem | `langchain-ai/langmem` | Open source | hot-path + background semantic/episodic/procedural memory workflows | Medium/High library |
| OpenMemory | `mem0ai/openmemory` | MIT | cross-coding-harness session portability | Early/beta; adjacent rather than core memory engine |
| SCIP | `scip-code/scip` | Open source | language-neutral precise symbol/reference exchange | High standard/tooling |
| Sourcegraph code graph | public Sourcegraph docs/source | mixed current Sourcegraph licensing; SCIP open | cross-repo precise navigation, semantic indexes | Very high |
| Zoekt | public GitHub | open source | extremely fast trigram code search | Very high for lexical code search |
| Joern / CPG | `joernio/joern`, CPG spec | open source | AST+CFG+data-flow-oriented code property graphs | Very high research/security |
| Aider RepoMap | `aider-ai/aider` implementation/docs | open source | Tree-sitter defs/refs + PageRank + token-budget repo map | High practical relevance |

### 4.2 Selection logic

Stars are intentionally not the deciding factor. These systems were selected because each solves a technically important part of SCOS's desired product:

- Hindsight: production memory retrieval/consolidation.
- Honcho: evolving user/agent representations.
- Graphiti: temporal truth and relationship invalidation.
- Letta: explicit memory-as-agent-state ergonomics.
- Cognee: storage/pipeline modularity and graph knowledge infrastructure.
- SCIP/Sourcegraph: precise semantic code indexes.
- Zoekt: scalable lexical code retrieval.
- Joern: deep program graphs.
- Aider: relevance-ranked repository skeleton under strict token budgets.

---

## 5. Competitor-by-Competitor Deep Dives

### 5.1 Hindsight

**Verified/Documented source:** `vectorize-io/hindsight` README, API docs, best-practices docs and integration skills.

Hindsight exposes three core operations: `retain`, `recall`, `reflect`.

Verified/documented mechanics:

- `retain` transforms content into structured facts, entities and relationships.
- recall runs **semantic, BM25, graph and temporal** strategies in parallel.
- per-strategy boosts and semantic similarity floor are configurable.
- observations are deduplicated, evidence-grounded abstractions supported by multiple memories and proof counts.
- mental models precompute answers for recurring reflection questions.
- banks isolate memory domains and support dispositions/missions/tags.
- later releases added VectorChord/external Postgres support, robust BM25 handling, concurrency serialization around retain, freshness checks, and richer outcomes.
- MCP, Python, Node, Go and agent integrations are productized.
- Hindsight publishes LongMemEval/agent-memory benchmark material and an open Agent Memory Benchmark effort.

**Why it matters to SCOS:** Hindsight's key advantage is not that it has more labels. It has a clearer **retrieval ensemble + evidence-grounded consolidation model**. SCOS should copy the principle, not its API names.

**Gap:** SCOS needs four-way retrieval with measurable contribution per strategy, an observation layer with exact evidence IDs, and reproducible memory benchmarks.

### 5.2 Honcho

**Verified/documented source:** `plastic-labs/honcho` README, docs and changelog.

Honcho is organized around:

- workspace
- peer
- session
- message
- conclusions
- representations
- peer cards
- summaries/context

Its distinctive model is **peer-centric and perspectival**. Internal collections are keyed by `(observer, observed)` peer pairs. That permits “what A believes/knows about B,” not merely a global flat user profile.

Background reasoning batches messages rather than necessarily reasoning on each message. Documentation describes token batching around roughly 1,000 pending tokens for a representation, reducing inference overhead. The changelog shows queue normalization, representation task deduplication, Redis caching, cache invalidation work, LLM provider fallback and finer metrics.

**Why it matters:** multi-agent systems require perspective. A shared statement made by Agent A should not automatically become trusted world truth for every agent.

**SCOS recommendation:** introduce `subject_id`, `observer_id`, `belief_scope`, `evidence_scope`, and trust/calibration fields on semantic conclusions. Do not force every raw memory through this model; use it for beliefs/representations.

### 5.3 Mem0

**Verified source:** `mem0ai/mem0/mem0/memory/main.py`; **Documented** managed-platform benchmark claims.

The inspected code shows `Memory` add flows that can directly embed and write message content when inference is disabled, and a broader configurable abstraction around embedding/vector stores. Mem0's public architecture historically used LLM-based memory extraction and mutation decisions; current 2026 documentation highlights a new managed algorithm with ADD-oriented extraction, entity linking, multi-signal semantic/BM25/entity retrieval and temporal reasoning.

Important caveat: the current README explicitly states that managed-platform benchmark scores include proprietary optimizations not present in the open-source SDK. Therefore those scores are not fair evidence of OSS implementation parity.

**Why it matters:** Mem0 is an integration/DX benchmark and a reminder that a simple public API can hide flexible storage/model adapters.

**SCOS recommendation:** beat Mem0 on evidence, time and code linkage, but match its “few-line integration” experience.

### 5.4 Letta / MemGPT

**Verified source:** Letta memory schemas and Letta Code memory filesystem prompts/docs.

Letta separates persistent editable **memory blocks** from out-of-context external/archival memory and conversation search. Memory blocks are limited, named prompt segments representing durable identity/user/working knowledge. External memory is retrieved on demand. Current Letta Code also projects memory into a local memory filesystem with Git-aware evolution.

**Why it matters:** the model gives the agent an understandable, inspectable memory hierarchy. SCOS currently has richer backend ambition but less obvious “what is always in context vs retrievable vs archival?” product semantics.

**SCOS recommendation:** expose a first-class context contract:

1. pinned working memory;
2. retrieved evidence;
3. synthesized representations;
4. task/repository map;
5. provenance/citation bundle.

### 5.5 Graphiti

**Verified source:** `graphiti_core/utils/maintenance/edge_operations.py`, `edges.py`, `search/search.py`, `search/search_config.py`.

Graphiti is one of the most important designs for SCOS.

Verified mechanisms include:

- episodic nodes and entity edges;
- entity-edge fields for `created_at`, `valid_at`, `invalid_at`, `expired_at`, `reference_time`, episode UUID evidence;
- extraction of relation timestamps relative to episode reference time;
- resolution of new edges against existing edges;
- invalidation of contradictory/overlapping temporal edges;
- search across full-text, similarity and BFS graph traversal;
- configurable RRF/MMR and graph-distance/episode-aware reranking;
- custom entity schemas and multiple graph backends.

This is **actual temporal relationship state**, not a timestamp metadata filter.

**SCOS gap:** explicit fact supersession is useful but narrower. SCOS needs valid-time intervals on relationships and claims, transaction-time/version lineage, and point-in-time query semantics.

### 5.6 Zep

Current Zep open-source Community Edition is documented as deprecated/legacy. Graphiti remains the relevant open implementation, while Zep Cloud is the current product surface. Zep documents relationship-aware temporal graph context and sub-200 ms hosted context claims, plus enterprise compliance.

**Audit rule:** do not benchmark SCOS open source against unverifiable hosted internals as if source-equivalent. Use Zep hosted behavior as a product/SLO target and Graphiti as the inspectable architecture baseline.

### 5.7 Cognee

**Verified/documented source:** Cognee architecture docs, repo examples/configuration.

Cognee explicitly separates:

- relational store for documents/chunks/provenance;
- vector store for embeddings;
- graph store for entities/relationships.

It supports local defaults and swappable providers. Public examples cover pgvector, LanceDB, Neo4j, Neptune, graph visualization, tenant/role/data access, ontology grounding, temporal examples, feedback, provenance and custom pipelines.

**SCOS lesson:** provider interfaces and pipeline composition are valuable. However, SCOS should avoid making three mandatory databases the default. One Postgres deployment can cover a surprising amount until graph scale/query depth justifies specialization.

### 5.8 LangMem

LangMem supports hot-path agent-managed memory plus background extraction/consolidation through a memory store manager and LangGraph storage.

**SCOS lesson:** expose both synchronous explicit memory writes and asynchronous automatic consolidation. The application must be able to select latency/cost guarantees instead of forcing one universal policy.

### 5.9 OpenMemory

As of the inspected 2026 repository, `mem0ai/openmemory` has pivoted to a CLI/TUI for porting coding sessions between Claude Code, Codex and OpenCode, with autosync and broader harness support on its roadmap.

This is adjacent rather than a sophisticated memory retrieval baseline, but it identifies a practical moat area: **portable agent history and configuration across harnesses**.

### 5.10 SCIP / Sourcegraph

SCIP is a language-neutral protobuf protocol for code intelligence. The schema represents documents, symbol information and external symbols, and is emitted by indexers for Java/Scala/Kotlin, TypeScript/JavaScript, Rust, C/C++, Ruby, Python, .NET, Dart, PHP and others.

Its significance is precision: definitions/references/implementations should come from semantic language tooling whenever available, not regex or filename inference.

**SCOS recommendation:** ingest SCIP as a first-class deterministic evidence source. Do not reimplement every compiler frontend.

### 5.11 Zoekt

Zoekt is a specialized indexed code-search engine built around trigram search and repository-aware indexing. Its role is not semantic reasoning; it is fast, predictable lexical retrieval.

**SCOS recommendation:** provide a sparse-search adapter interface. For local/small deployments PostgreSQL FTS/trigram can be sufficient; at very large code scale, a Zoekt adapter is more sensible than rebuilding a world-class lexical engine.

### 5.12 Joern / Code Property Graph

Joern builds Code Property Graphs, an extensible cross-language intermediate representation used for static analysis. CPGs combine structural program representations and analysis overlays, allowing queries beyond simple symbol references.

**SCOS recommendation:** adopt CPG concepts selectively. Full Joern-equivalent static analysis for every language is expensive. SCOS should model AST, call, type, control-flow and data-flow edge families, but only promise deterministic precision where an adapter provides it.

### 5.13 Aider RepoMap

Aider's repo map extracts definitions/references using Tree-sitter, creates a graph between referencers and definers, ranks it using PageRank with context-sensitive weighting, then emits a token-budget-limited repository skeleton.

This is directly relevant to SCOS context assembly. A giant graph is useless if context selection ignores graph centrality and the active task.

**SCOS recommendation:** compute query-conditioned graph centrality/subgraph ranking as one context feature, not a static global PageRank only.

---

## 6. Detailed Competitive Feature Matrix

Status: **Strong / Partial / Basic / Missing / Unknown**.

| Capability | SCOS | Hindsight | Honcho | Mem0 | Letta | Graphiti | Cognee | LangMem | SCIP/Sourcegraph | Joern/Aider | Best observed pattern |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Fact extraction | Partial | Strong | Strong | Strong | Partial | Strong | Strong | Strong | N/A | N/A | Hindsight/Graphiti evidence extraction |
| Explicit memory writes | Strong | Strong | Strong | Strong | Strong | Strong episode add | Strong | Strong | N/A | N/A | all mature systems |
| Automatic background memory | Partial | Strong | Strong | Strong | Partial | Strong | Strong | Strong | N/A | N/A | Honcho/LangMem |
| Fact version lineage | Strong | Strong | Partial | Partial | Partial | Strong temporal | Partial | Partial | N/A | N/A | Graphiti temporal edges |
| Bi-temporal validity | Missing | Partial/Strong documented | Partial | Partial documented | Missing | **Strong** | Partial | Missing | commit time only | snapshot analyses | Graphiti |
| Evidence/provenance | Partial | **Strong** observations/proofs | Partial | Partial | Partial | **Strong** episode links | Strong | Partial | **Strong deterministic index origin** | Strong source locations | Hindsight + Graphiti + SCIP |
| Entity resolution | Missing/Partial | Strong | peer-centric | Strong current managed docs | Basic | Strong | Strong | Partial | symbols are identities | code identities | Graphiti/Cognee |
| Relationship graph | Partial external | Strong | representation graph conceptual | graph option | Basic | **Strong** | **Strong** | Basic | symbol refs | **Strong CPG** | Graphiti for memory, Joern for code |
| Semantic retrieval | Partial | Strong | Strong | Strong | Strong archival | Strong | Strong | store-dependent | N/A | N/A | Hindsight |
| BM25 / lexical | Missing/Unknown | **Strong** | Strong hybrid docs | Strong managed docs | hybrid conversation | **Strong** | Strong options | store-dependent | Zoekt side | code text via tools | Hindsight/Graphiti + Zoekt |
| Graph retrieval | Partial | Strong | Basic | Partial | Basic | **Strong** | Strong | Basic | code graph nav | Strong | Graphiti/Joern |
| Temporal retrieval | Partial metadata | **Strong** | evolving representations | Strong managed docs | Basic | **Strong** | Partial | Basic | commit filters | snapshot based | Graphiti/Hindsight |
| Retrieval fusion/RRF | Missing/Unknown | Strong strategy ensemble | Unknown details | managed multi-signal | Partial | **Strong configurable RRF** | hybrid | store-dependent | composition outside SCIP | Aider graph rank | Graphiti |
| MMR/diversification | Missing/Unknown | Unknown | Unknown | Unknown | Unknown | **Strong configurable** | Unknown | Unknown | N/A | N/A | Graphiti |
| Evidence-grounded consolidation | Partial merge | **Strong observations** | **Strong conclusions** | Partial | agent-managed blocks | edge resolution | Strong pipelines | Strong manager | N/A | N/A | Hindsight/Honcho |
| Mental/precomputed models | Missing | **Strong** | peer cards/representations | Missing | memory blocks | communities | graph summaries | Missing | N/A | repo maps | Hindsight/Honcho |
| Observer→observed beliefs | Missing | Missing | **Strong** | Missing | Missing | group/entity based | Missing | Missing | N/A | N/A | Honcho |
| Working/core memory UX | Partial | Partial | representations | Partial | **Strong** | N/A | Partial | Partial | N/A | repo map | Letta |
| Multi-user isolation | Strong direction | banks | workspaces | filters/users | server scopes | group IDs | tenant/ACL support | store namespaces | repo permissions external | external | enterprise systems |
| Deletion cascade proof | Missing | Unknown | Unknown | Partial | Partial | graph deletion available | prune/forget | store dependent | index deletion | snapshot rebuild | none clearly dominant |
| Code semantic symbols | **Weak** | N/A | N/A | N/A | N/A | N/A | N/A | N/A | **Strong** | Strong-ish Tree-sitter | SCIP |
| Compiler/LSP precision | Missing | N/A | N/A | N/A | N/A | N/A | N/A | N/A | **Strong** | Joern language frontends | SCIP/Sourcegraph |
| Lexical code search | Basic | N/A | N/A | N/A | N/A | N/A | N/A | N/A | Sourcegraph/Zoekt | file tools | Zoekt |
| AST graph | Partial external | N/A | N/A | N/A | N/A | N/A | N/A | N/A | partial index data | **Strong Joern/Tree-sitter** | Joern |
| CFG/data flow | Missing | N/A | N/A | N/A | N/A | N/A | N/A | N/A | indexer dependent | **Strong Joern** | Joern |
| Token-budget repo map | Partial context service | N/A | N/A | N/A | context blocks | N/A | N/A | N/A | product layer | **Strong Aider** | Aider |
| Commit-aware code history | Basic | N/A | N/A | N/A | memory git history | N/A | source provenance | N/A | snapshots/revisions | snapshot analyses | opportunity for SCOS |
| Unified human history + code graph | **Partial architecture** | Missing | Missing | Missing | Partial coding history | Missing | general graph possible | Missing | code only | code only | **SCOS moat opportunity** |

---

## 7. Persistent Memory Analysis

### 7.1 Required canonical memory layers

SCOS should stop treating “memory” as one table with optional type labels. A market-leading system needs explicit layers with different lifecycle rules:

1. **Evidence / episode**: immutable raw or normalized source event.
2. **Atomic claim**: subject-predicate-object/value assertion tied to evidence.
3. **Semantic memory**: durable generalized knowledge.
4. **Episodic memory**: event with participants, place/context and event time.
5. **Procedural memory**: rule/procedure/skill with applicability conditions and outcome evidence.
6. **Preference/profile memory**: subject-specific preferences with confidence and temporal validity.
7. **Working/pinned memory**: explicitly bounded material always eligible for context.
8. **Observation/conclusion**: synthesized abstraction supported by multiple evidence items.
9. **Archive**: retained evidence not normally retrieved unless specifically requested.

### 7.2 Creation

Each write should support:

- explicit agent/user memory write;
- automatic extraction from conversation/tool/action events;
- deterministic event ingestion from Git, issues, PRs, tests and execution traces;
- background consolidation.

Every generated claim stores:

- extractor type + version;
- model/provider/version if probabilistic;
- prompt/template hash;
- source evidence IDs;
- confidence/calibration bucket;
- event time and ingestion time;
- namespace/tenant/subject/observer;
- privacy and retention policy.

### 7.3 Contradictions

Do not immediately overwrite old facts. Model competing claims.

Example:

```text
Claim A: user.prefers_editor = VS Code
valid: 2026-01-01 → 2026-05-12
Claim B: user.prefers_editor = Zed
valid: 2026-05-12 → open
```

Resolution algorithm:

1. deterministic identity/normalization;
2. semantic contradiction classifier only for candidate claims;
3. temporal ordering;
4. source trust weighting;
5. explicit user correction outranks inferred behavior;
6. mark previous claim `valid_to`, do not destroy evidence;
7. retain ambiguity when evidence cannot support a winner.

### 7.4 Consolidation

Hindsight's observation idea is worth adopting in a more general evidence model.

An observation should contain:

- normalized conclusion;
- support count;
- contradiction count;
- evidence IDs;
- first/last supported time;
- confidence distribution;
- last consolidation run;
- source diversity;
- scope;
- embedding/index version.

Never let a synthesized observation become its own untraceable evidence source. The provenance DAG must prevent circular self-reinforcement.

### 7.5 Forgetting and decay

Use **type-aware decay**, not one global half-life.

- identity facts: no passive decay; invalidate by contradiction.
- temporary preferences: moderate decay.
- project decisions: decay only when superseded or code/evidence changes.
- working context: short TTL.
- inferred behavioral observations: confidence decay unless reinforced.
- security-sensitive data: policy TTL regardless of access.

Retrieval score should not directly mutate truth confidence merely because something is frequently retrieved. Popular mistakes are still mistakes. Track **retrieval utility** separately from **epistemic confidence**.

---

## 8. Code Indexing Analysis

### 8.1 Current problem

The current code-intelligence service has useful domain types but insufficient extraction and storage. Filename/path conventions cannot power claims like “who calls this method?” with acceptable precision.

### 8.2 Required multi-tier parser architecture

#### Tier A: deterministic semantic indexers

Prefer when available:

- SCIP TypeScript
- SCIP Java/Scala/Kotlin
- rust-analyzer SCIP
- SCIP Python
- scip-clang
- SCIP .NET
- Go semantic indexer / `go/packages` + `go/types` adapter
- language-server or compiler-native adapters where SCIP output is unavailable

Outputs:

- symbol identity
- definition ranges
- references
- implementations
- signatures/types
- documentation
- package/module identity
- dependency provenance

#### Tier B: Tree-sitter structural indexer

For broad language coverage:

- functions/methods/classes/interfaces
- imports/exports
- calls where syntactically evident
- test/config/routes/schema patterns
- syntax-aware chunks

#### Tier C: semantic/LLM enrichment

Only after deterministic structure exists:

- architectural role
- likely business capability
- implicit cross-service relationship
- inferred test relationship
- domain concepts
- decision rationale candidates

All Tier C edges must be marked probabilistic.

### 8.3 Chunking

Stop using generic fixed-size chunks as the primary code unit.

Store:

- symbol body chunk
- signature/doc chunk
- file synopsis
- class/module synopsis
- call-neighborhood synopsis
- commit-diff chunk
- test relationship chunk

Parent/child linkage allows retrieval to start precise and expand only when budget permits.

### 8.4 Precise identity

Symbol IDs must not depend only on file path + display name. Use a stable identity structure:

```text
repo_id
language
package/module identity
semantic symbol descriptor
signature/overload discriminator
```

Separately maintain per-snapshot occurrence IDs because a stable conceptual symbol can move or change signature.

---

## 9. Code Graph Analysis

### 9.1 Target node families

`Repository`, `Branch`, `Commit`, `Directory`, `File`, `Package`, `Module`, `Class`, `Interface`, `Function`, `Method`, `Symbol`, `Variable`, `Constant`, `Type`, `Schema`, `Table`, `Column`, `APIEndpoint`, `Job`, `Queue`, `Event`, `Test`, `Documentation`, `Configuration`, `ExternalService`, `Dependency`, `Author`, `Issue`, `PullRequest`, `Requirement`, `Decision`, `Incident`, `AgentAction`.

### 9.2 Target edge families

`CONTAINS`, `DEFINES`, `IMPORTS`, `EXPORTS`, `CALLS`, `REFERENCES`, `IMPLEMENTS`, `EXTENDS`, `OVERRIDES`, `INSTANTIATES`, `RETURNS`, `ACCEPTS`, `READS`, `WRITES`, `MUTATES`, `DEPENDS_ON`, `TESTS`, `DOCUMENTS`, `CONFIGURES`, `PUBLISHES`, `CONSUMES`, `ROUTES_TO`, `USES_SCHEMA`, `MODIFIED_BY`, `INTRODUCED_IN`, `REMOVED_IN`, `RENAMED_FROM`, `RELATED_TO`.

### 9.3 Deterministic vs probabilistic edges

| Edge | Default extraction | Confidence policy |
|---|---|---|
| CONTAINS / DEFINES | parser/indexer | deterministic 1.0 |
| IMPORTS / EXPORTS | semantic parser | deterministic 1.0 when resolved |
| REFERENCES | SCIP/compiler/LSP | deterministic/high |
| IMPLEMENTS / EXTENDS / OVERRIDES | compiler/type system | deterministic/high |
| CALLS | compiler where statically resolvable; syntax otherwise | high or bounded probabilistic |
| READS / WRITES / MUTATES | data-flow/static analysis | deterministic only if analysis proves it |
| TESTS | framework/static relationship + coverage | evidence-weighted |
| ROUTES_TO | framework adapters/config | high if deterministic pattern |
| PUBLISHES / CONSUMES | framework/event adapters | high if resolved |
| RELATED_TO | LLM/embedding | explicitly probabilistic |
| decision→symbol | evidence linkage | high if commit/PR evidence, lower if inferred |

### 9.4 Edge schema

Every edge must include:

- edge ID
- tenant/workspace/repo namespace
- source node ID / target node ID
- relation type
- valid-from / valid-to
- observed-at / ingested-at
- snapshot/commit provenance
- extraction method
- extractor version
- source locations/evidence IDs
- confidence score
- confidence class (`deterministic`, `static-analysis`, `heuristic`, `model-inferred`)
- status (`active`, `invalidated`, `tombstoned`)

This is how SCOS can fuse deterministic and semantic graphs without pretending an LLM guess is compiler truth.

---

## 10. Retrieval Analysis

### 10.1 Target retrieval fabric

For each query, classify intent and fan out only to relevant retrievers:

1. exact symbol/identity lookup;
2. lexical code search (FTS/trigram/Zoekt);
3. sparse memory/document search (BM25);
4. dense semantic retrieval;
5. graph neighborhood/path search;
6. temporal search;
7. Git/history search;
8. structured metadata filters.

Normalize scores per retriever, fuse with RRF or learned/calibrated fusion, then rerank top candidates with a small cross-encoder or model only when the query warrants it.

### 10.2 Query-specific recipes

#### “Where is authentication implemented?”

- lexical `auth`, `authentication`, middleware terms;
- symbol lookup for auth modules/types;
- route/middleware graph;
- semantic search over symbol synopses;
- rank central auth-related modules;
- return exact files/symbols with definitions and inbound uses.

#### “What code would break if I modify this interface?”

- resolve interface symbol precisely;
- implementations + references + accepts/returns edges;
- transitive dependency traversal with bounded depth;
- tests covering affected symbols;
- build/package dependency graph;
- historical co-change signal;
- produce risk ranked by deterministic evidence first.

#### “Why does this function exist?”

- symbol definition + callers;
- introducing commit;
- PR/issue linked to commit;
- conversation/decision memories linked to those artifacts;
- previous versions/diff;
- answer with evidence chain.

#### “What changed in this subsystem over the last six months?”

- map subsystem → symbols/files;
- commit diff graph within interval;
- symbol lineage/renames;
- linked PRs/issues/decisions;
- summarize changes by capability and behavioral impact.

#### “Which tests cover this function?”

- explicit static references from test symbols;
- dynamic coverage evidence if available;
- mock/fixture relationships;
- rank direct coverage above inferred semantic similarity.

#### “What did the user tell the agent three months ago?”

- temporal window constraint first;
- episodic evidence retrieval;
- semantic memory only as expansion;
- do not answer solely from a later consolidated profile when original-period evidence exists.

### 10.3 Ranking feature model

Candidate features should include:

- exact identity match
- lexical score
- dense similarity
- graph distance
- relation type prior
- path centrality
- query/repository locality
- same branch/commit recency
- temporal fit
- source trust
- epistemic confidence
- retrieval utility
- access frequency (small weight only)
- importance/salience
- evidence diversity
- user/task personalization

Keep the scoring trace so every result can explain why it ranked.

---

## 11. Temporal and Git-Aware Intelligence

### 11.1 Required snapshot model

Use an immutable base snapshot per indexed commit plus branch pointers/overlays.

Do not mutate one “current repo graph” in place without lineage.

Core objects:

- `repository`
- `commit`
- `branch_ref`
- `index_snapshot`
- `file_version`
- `symbol_version`
- `edge_version`

### 11.2 Incremental pipeline

For a changed checkout/commit:

1. read Git diff against indexed parent/base;
2. detect added/deleted/modified/renamed files using Git similarity plus content hashes;
3. hash normalized source bytes;
4. skip exact unchanged files;
5. parse changed files;
6. compute structural AST hash;
7. derive semantic symbol fingerprints;
8. diff old/new symbols;
9. tombstone removed occurrences;
10. create rename/move lineage where confidence is high;
11. update local deterministic edges;
12. enqueue dependants whose semantic resolution may change;
13. update lexical/vector indexes only for affected chunks;
14. update graph indexes;
15. mark snapshot complete atomically;
16. move branch pointer to completed snapshot.

### 11.3 Dependency ripple invalidation

A changed exported symbol may invalidate more than its file.

Maintain an invalidation queue keyed by edge semantics:

- signature change → callers/implementers/importers;
- type change → users + serializers + schema adapters;
- schema migration → model/query/API dependencies;
- route contract → clients/tests/docs;
- configuration key → readers + deployment manifests.

Use a cap/budget to prevent pathological whole-repo waves, and fall back to background full reconciliation when threshold is exceeded.

### 11.4 Graphify cache defect migration

Replace current existence-only cache checks with:

```text
cache key = repo_id + snapshot_id + extractor_version + config_hash
```

A graph for commit A can never be served as commit B because the identity itself changes. Garbage collection can remove unreachable old generations later.

---

## 12. Unified Memory + Code Knowledge Architecture

The core SCOS differentiator should be a **Unified Evidence Graph (UEG)**.

### 12.1 Cross-domain links

```text
User ──HAS_PREFERENCE──> MemoryClaim
MemoryClaim ──SUPPORTED_BY──> ConversationTurn
ConversationTurn ──CREATED_DECISION──> Decision
Decision ──DRIVES──> Requirement
Requirement ──IMPLEMENTED_BY──> PullRequest
PullRequest ──CONTAINS──> Commit
Commit ──MODIFIES──> SymbolVersion
SymbolVersion ──CALLS──> SymbolVersion
Test ──TESTS──> SymbolVersion
Incident ──INVOLVES──> SymbolVersion
AgentAction ──MUTATES──> RepositorySnapshot
```

### 12.2 Stale-memory invalidation

This is a major moat opportunity.

If memory says:

> “Authentication is handled in `auth/middleware.go` by `ValidateSession`.”

and a later commit removes that symbol, SCOS should:

1. detect symbol tombstone;
2. find claims whose evidence/subject links reference it;
3. mark those claims `possibly_stale`;
4. attempt deterministic re-resolution to renamed/moved successor;
5. if resolved, create a new claim version and maintain lineage;
6. if not, lower retrieval eligibility for current-state questions while preserving historical validity.

No generic memory system can do this without a code graph and evidence links.

---

## 13. Storage and Infrastructure Analysis

### 13.1 Recommended topology

#### Local / embedded mode

- SQLite or libSQL for canonical metadata/evidence
- FTS5 for lexical search
- embedded vector extension or LanceDB adapter
- adjacency tables/materialized graph indexes
- object/filesystem store for snapshots

Goal: one-command local use, no Docker required.

#### Default production mode

- PostgreSQL 16+
- pgvector/VectorChord-compatible vector adapter
- PostgreSQL FTS + `pg_trgm`
- relational adjacency/closure/path helper tables for bounded graph traversal
- object storage (S3-compatible) for raw repos, index artifacts, large evidence
- Redis/Valkey optional for cache/queue coordination

Goal: one operational database for most installations.

#### Scale-out mode

Adapters for:

- Zoekt/OpenSearch for huge lexical corpora
- dedicated vector engine when needed
- Neo4j/FalkorDB/Memgraph/graph engine for deep traversal at very large edge counts
- Kafka/NATS/Redpanda or durable DB queue for high-volume ingestion

### 13.2 Why not force multiple databases immediately?

Cognee's multi-store architecture is capable, but operational complexity is real. Most customers do not want to operate Postgres + graph DB + vector DB + search engine + queue before their first useful query.

SCOS should design **interfaces for specialization but a strong consolidated default**.

### 13.3 Canonical-store rule

Specialized indexes are disposable projections. PostgreSQL/SQLite canonical evidence is authoritative.

If a vector or graph index is corrupted, it must be rebuildable from canonical versions/evidence without semantic loss.

---

## 14. Performance and Scalability

### 14.1 Target SLOs

Targets should be measured on published hardware classes.

| Operation | Initial target | Stretch target |
|---|---:|---:|
| explicit memory write, durable ack | p95 < 100 ms excluding model extraction | < 50 ms |
| background extracted memory availability | p95 < 3 s typical turn | < 1.5 s |
| simple memory retrieval | p95 < 150 ms pre-reranker | < 80 ms |
| hybrid memory retrieval + rerank | p95 < 500 ms | < 250 ms |
| exact symbol lookup | p95 < 30 ms | < 15 ms |
| lexical code query | p95 < 100 ms | < 50 ms |
| 1–2 hop graph neighborhood | p95 < 100 ms | < 50 ms |
| changed-file incremental index | p95 < 2 s/file language dependent | < 750 ms |
| branch snapshot visibility after small commit | p95 < 10 s | < 3 s |
| stale-index serving | **0 accepted** | **0** |

### 14.2 Scale tiers to prove

- 10k memories / 10k symbols
- 1M memories / 1M symbols / 10M edges
- 10M memories / 10M symbols / 100M edges
- monorepo with >1M files as stress tier where applicable
- 100 concurrent agents
- 1,000 retrieval QPS synthetic index tier

### 14.3 Current bottlenecks

1. JSON snapshot code store: O(corpus) persistence and process-memory residency.
2. convention-based extraction: cheap but low quality.
3. Graphify single worker: bounded memory but indexing throughput constrained.
4. Graphify no-cluster mode: acceptable for current handlers but limits graph summarization/community use.
5. stale graph cache identity.
6. service-to-service overhead and deployment drift if each thin function remains independently deployed.

---

## 15. Evaluation and Benchmarking

### 15.1 Memory benchmarks

Use at least:

- LongMemEval
- LoCoMo
- BEAM if licensing/data and methodology permit current reproduction
- a SCOS temporal contradiction suite
- a privacy/deletion suite
- project-history benchmark built from real public repos/issues/PRs

| Task | Format | Metric | Baseline | Target |
|---|---|---|---|---|
| factual recall | long conversation → question | exact/F1 + judged support | Hindsight/Mem0 published where comparable | parity/better with cited evidence |
| temporal recall | evolving facts | temporal QA accuracy | Graphiti/Hindsight | >95% on deterministic suite |
| contradiction | correction sequence | latest/current + history accuracy | own naive vector baseline | >98% simple corrections |
| preference retention | multi-session profile | precision/recall | vector baseline | +15% F1 |
| irrelevant suppression | distractor memories | precision@k, answer faithfulness | dense-only | >20% precision lift |
| forgetting | delete request | zero-retrieval + physical index verification | current SCOS | 100% logical, auditable physical cascade |
| provenance | answer → evidence | evidence precision | no-citation baseline | >98% supported claims |

### 15.2 Code benchmarks

- CodeSearchNet for semantic code search where useful.
- RepoBench / CrossCodeEval-style cross-file reasoning tasks where current datasets remain valid.
- SWE-bench Verified for end-to-end agent outcomes, not as a pure index benchmark.
- compiler/LSP-generated ground truth for definitions/references.
- synthetic mutation benchmark for impact analysis.
- public-repo history benchmark for symbol lineage and change reasoning.

| Task | Expected ground truth | Metric | Target |
|---|---|---|---|
| definition lookup | compiler/LSP definition | accuracy@1 | >99% supported languages |
| reference lookup | compiler/LSP references | precision/recall | >98% / >97% |
| implementation lookup | type system | F1 | >97% |
| call graph | static-analysis baseline | edge precision/recall by language | publish per language |
| test discovery | static refs + dynamic coverage | recall@10 | >95% direct tests |
| bug localization | benchmark issue → files/symbols | MRR/recall@k | beat dense-only by >15% |
| impact prediction | mutation → failing packages/tests | precision/recall | >90% precision with tiered risk |
| rename lineage | git move/refactor corpus | lineage accuracy | >95% high-confidence cases |
| architecture QA | repo question → evidence | judged correctness + citation precision | >90% supported claims |

### 15.3 Ablations

Every serious benchmark must publish ablations:

- dense only
- BM25 only
- exact symbols only
- graph only
- temporal only
- hybrid without reranker
- hybrid + reranker
- hybrid + graph expansion
- hybrid + memory/code unified evidence

Without ablation, “hybrid is better” is marketing with a lab coat.

### 15.4 End-to-end agent benchmark

Run identical coding agent tasks with:

A. no SCOS  
B. SCOS memory only  
C. SCOS code index only  
D. SCOS unified graph  
E. competitor memory + same agent where integration permits.

Measure:

- task completion
- SWE-bench pass rate
- tokens
- tool calls
- wall time
- wrong-file edits
- reverted edits
- tests run before correct patch
- hallucinated symbols
- retrieval precision
- historical-decision compliance

---

## 16. Developer Experience

### Competitive DX requirements

SCOS needs:

- one binary / `scos init`
- Docker Compose for full local stack
- embedded mode with zero external services
- Python, TypeScript and Go SDKs
- MCP server
- Claude Code/Codex/OpenCode/Hermes integrations
- auto-discovery/configuration adapters
- web admin UI
- memory inspector with evidence/version timeline
- graph explorer
- code symbol explorer
- retrieval trace (“why this context?”)
- indexing job UI
- deletion/retention controls
- health/release fingerprint panel

### API design

Keep the advanced internals behind a small surface:

```text
remember(event|messages|document)
recall(query, scope, as_of?)
explain(memory_or_answer_id)
forget(selector)
index_repository(repo, branch?)
search_code(query, snapshot?)
impact(symbol|diff)
context(task, budget)
```

Experts can use low-level graph/search APIs; normal agents should not need to know which database is serving the answer.

---

## 17. Security and Enterprise Readiness

### 17.1 Current positives

- tenant-aware request handling appears in inspected services;
- code-intelligence contains explicit commentary/fixes around tenant filtering;
- Graphify invocation avoids shell execution and blocks flag-like argument injection;
- memory writes use transactional locking;
- database TLS helpers exist;
- credential/governance/backup services indicate security is treated as a product concern.

### 17.2 Blockers

1. **No clearly verified repository license at SCOS repo metadata level.** If public adoption is desired, add an explicit license.
2. Uniform tenant isolation must be proven by cross-tenant negative integration tests for every endpoint/index/cache.
3. Derived-index deletion must be auditable.
4. Encryption-at-rest strategy for embeddings/graphs needs explicit guarantees; “encrypted flag” alone is not a system.
5. Secrets and model-provider credentials require scoped vault identities and rotation.
6. Queue/idempotency behavior must survive retries without duplicate semantic mutations.
7. Schema migrations require forward/backward compatibility policy.
8. Backup restore drills must verify canonical data **and rebuilt indexes**.
9. Release parity must be enforced, not inferred.
10. SLOs need error budgets, not just telemetry endpoints.

### 17.3 Required security invariants

- Every row/node/index document carries tenant namespace or is physically isolated.
- Cache keys include tenant + authorization-relevant scope.
- Service identity is cryptographically authenticated internally.
- No client-supplied tenant header is trusted without gateway/auth binding.
- Every retrieved item is authorization-filtered before reranking/context assembly.
- Deletion creates a durable deletion event and tombstones all derived projections until physical cleanup is confirmed.
- Backup retention must respect deletion/legal policies.

---

## 18. Technologies Competitors Use That We Don't

| Technology / pattern | Used by | Purpose | Why it matters | Current SCOS equivalent | Gap | Action |
|---|---|---|---|---|---|---|
| bi-temporal entity edges | Graphiti | evolving truth | current vs historical correctness | supersession timestamps | substantial | **Adopt model** |
| episode-linked edge evidence | Graphiti | provenance | explains relationship origin | source session/turn | partial | **Adopt** |
| BM25 + dense + graph + temporal ensemble | Hindsight | recall quality | complementary failure modes | partial retrieval signals | major | **Implement** |
| observations with proof counts | Hindsight | evidence-grounded consolidation | stable abstraction without losing evidence | merges | major | **Implement** |
| mental models / precomputed reflections | Hindsight | low-latency repeated synthesis | useful for profiles/common questions | none verified | moderate | Adopt selectively |
| observer→observed collections | Honcho | perspectival beliefs | multi-agent/social cognition | flat user/tenant facts | major for swarm use | **Implement as optional belief layer** |
| token-batched background reasoning | Honcho | inference cost control | reduces per-message LLM churn | background architecture unclear | moderate | Adopt |
| Redis cache/task normalization | Honcho | throughput | production reliability | distributed service fabric | partial | Adopt only where load requires |
| memory blocks | Letta | explicit always-in-context state | inspectability/control | context contracts | UX gap | Adopt concept, not storage design |
| SCIP protocol | Sourcegraph ecosystem | precise symbols/references | compiler-grade navigation | convention extractors | critical | **Adopt** |
| Tree-sitter | Aider/others | broad structural parse | many-language coverage | Graphify external + heuristics | critical | **Adopt directly** |
| trigram code index | Zoekt | fast lexical search | code identifiers/substrings | map scans/unknown | major | Postgres trigram first; adapter later |
| Code Property Graph | Joern | AST/CFG/data-flow fusion | deep impact/security analysis | simple relation graph | major | Reimplement compatible concepts selectively |
| PageRank repo map | Aider | budgeted context relevance | better context under token limit | context optimisation | likely gap | Adopt query-conditioned variant |
| relational+vector+graph adapters | Cognee | backend flexibility | local→enterprise scale | Postgres + service integrations | partial | Formalize adapters |
| ontology-constrained extraction | Cognee/Graphiti | controlled schemas | fewer arbitrary entity types | no verified equivalent | moderate | Adopt for domain packs |
| dynamic pipeline tasks | Cognee | extensibility | custom ingestion | service architecture | partial | Build typed pipeline SDK |
| managed benchmark disclosure | Hindsight/Mem0 | credibility | measurable quality | harness but weak public suite | critical | Build/open benchmark suite |

---

## 19. What They Have That We Don't

This section separates useful gaps from shiny distractions.

### 19.1 Graphiti: real temporal edge validity

- **Feature:** valid/invalid intervals on relationships plus episode evidence.
- **Exact implementation:** `graphiti_core/edges.py` `EntityEdge`; `utils/maintenance/edge_operations.py` resolves extracted edges and invalidates overlapping/contradictory edges.
- **Value:** Very high.
- **Marketing?** No. This directly changes correctness of current-vs-historical answers.
- **Difficulty:** L.
- **Dependencies:** schema migration, temporal query planner, extraction/resolution.
- **SCOS implementation:** introduce `claim_version` and `relationship_version` with valid time + transaction time; never overload `updated_at` to mean both.
- **Priority:** P0/P1.

### 19.2 Graphiti: configurable hybrid graph search recipes

- full-text, embedding, BFS and rerank strategies;
- RRF/MMR and graph-aware rerank configuration;
- concrete source: `graphiti_core/search/search.py`, `search_config.py`, `search_utils.py`.
- **Value:** High.
- **SCOS:** build retrieval plan DSL and contribution tracing.
- **Priority:** P1.

### 19.3 Hindsight: four parallel memory retrieval strategies

- semantic + BM25 + graph + temporal;
- tunable per-strategy boosts;
- **Value:** High, measurable.
- **SCOS:** current recency/access scoring is too narrow.
- **Priority:** P1.

### 19.4 Hindsight: observations with evidence/proof counts

- consolidated belief grounded in exact supporting memories, refined rather than blindly overwritten;
- **Value:** High.
- **SCOS:** merge log is not equivalent to multi-evidence abstraction.
- **Priority:** P1.

### 19.5 Hindsight: mental models

- precomputed reflect answers for common questions/profiles;
- **Value:** Medium/High where repeated retrieval dominates.
- **Risk:** stale synthesis.
- **SCOS:** only enable with dependency/evidence invalidation.
- **Priority:** P2.

### 19.6 Honcho: perspectival peer model

- internal representation keyed by observer/observed peer pairs;
- **Value:** Transformational for multi-agent SCOS/swarm scenarios, unnecessary for simple single-user memory.
- **SCOS:** belief namespace and trust edges.
- **Priority:** P2.

### 19.7 Honcho: background reasoning batches

- queue batches messages by token threshold before representation work;
- **Value:** cost/throughput.
- **SCOS:** durable consolidation scheduler with token/cost policies.
- **Priority:** P1.

### 19.8 Letta: explicit memory block UX

- named bounded blocks always in context; external memory searched separately;
- **Value:** High DX, not inherently superior backend intelligence.
- **SCOS:** expose pinned memory/context packs.
- **Priority:** P2.

### 19.9 SCIP/Sourcegraph: precise cross-language semantic indexes

- compiler/LSP-generated definitions, references, implementations and external symbols;
- **Value:** Critical.
- **SCOS:** largest code-intelligence gap.
- **Difficulty:** L if ingesting SCIP; XL if reimplementing indexers. **Therefore ingest SCIP.**
- **Priority:** P0.

### 19.10 Zoekt: dedicated high-performance lexical code index

- **Value:** High at scale; lower at small repo scale.
- **SCOS:** begin with Postgres FTS/trigram, adapter to Zoekt for scale.
- **Priority:** P1/P2.

### 19.11 Joern: code property graph depth

- AST plus semantic/static-analysis overlays including control/data-flow concepts.
- **Value:** High for change impact, security, deep program reasoning.
- **Difficulty:** XL.
- **SCOS:** do not clone Joern wholesale. Support CPG-compatible edge families and ingest analysis output from specialized adapters.
- **Priority:** P2/P3.

### 19.12 Aider: PageRank under a token budget

- defs/refs → graph → PageRank → greedy budget fill;
- **Value:** High relative to implementation cost.
- **Difficulty:** S/M.
- **SCOS:** query-conditioned centrality using current task/seeds.
- **Priority:** P1.

### 19.13 Cognee: formalized multi-store adapter ecosystem

- **Value:** Medium/High for deployment flexibility.
- **SCOS:** formal interfaces and conformance tests, but retain a single-Postgres default.
- **Priority:** P2.

### 19.14 Cognee: ontology-constrained graph extraction

- **Value:** High in enterprise/domain settings, modest for general chat.
- **SCOS:** schema packs for code, infrastructure, product, user profile, incidents.
- **Priority:** P2.

### 19.15 Public reproducible benchmarks

- Hindsight and Mem0 make benchmark performance central to product claims; Hindsight has open benchmark efforts.
- **Value:** Critical commercially.
- **SCOS:** benchmark harness package is not enough. Publish datasets, configs, model versions, costs, latency and raw outputs.
- **Priority:** P0.

---

## 20. What We Have That They Don't

Only defensible advantages are listed.

### 20.1 A product boundary that already includes both memory and code intelligence

Most memory products do not own a precise code graph. Most code intelligence products do not own a user's cross-session autobiographical/project memory.

SCOS already has service boundaries for both. That is a **potential** differentiator, not yet a quality advantage.

**Productize by:** creating canonical cross-domain IDs and evidence links rather than keeping memory and code as neighboring APIs.

### 20.2 Explicit fact supersession and merge audit mechanics in the inspected memory store

SCOS has concrete `supersedes_id`, `superseded_by_id`, merge logging and transactional locking. Some competitors have richer semantic models, but this foundation is solid.

**Strengthen by:** adding valid-time intervals, contradiction sets and evidence DAGs.

### 20.3 Impact-reconciliation concept in code intelligence

The `ImpactReconciliation` model records predicted risk/dependants vs actual dependants and build/test outcomes. That is exactly the sort of feedback loop needed for self-improving impact scoring.

**Caveat:** the current underlying graph is not precise enough to make this a moat yet.

**Productize by:** collect predictions/outcomes and train/calibrate language/repo-specific risk models once deterministic graph quality is adequate.

### 20.4 Broad agent/platform fabric

SCOS's gateways/providers/agent registry/control-plane direction is broader than a standalone memory library.

**Risk:** breadth becomes deployment overhead if every named service is shallow.

**Strengthen by:** consolidate processes where scale/security do not require independent deployment, while preserving package boundaries.

---

## 21. Architectural Problems in scos-memory

### A1. JSON-backed code store

- **Current:** in-memory maps serialized to one JSON file.
- **Severity:** Critical.
- **Trigger:** medium repos / multiple processes / frequent updates.
- **Failure:** memory growth, full-state writes, weak querying, no distributed concurrency.
- **Superior:** canonical DB + immutable snapshots + secondary indexes.
- **Migration:** dual-write existing model into new schema; validate counts/hashes; switch reads; retain JSON importer.

### A2. Heuristic/convention symbol extraction

- **Current:** language functions infer package/module symbols from path/name conventions.
- **Severity:** Critical for code correctness.
- **Trigger:** any question requiring real references/types/calls.
- **Superior:** SCIP/compiler/LSP + Tree-sitter fallbacks.
- **Migration:** retain existing adapter interface, change implementation and capability reporting.

### A3. Graph cache without snapshot identity

- **Current:** cached graph returned when file exists; source staleness not checked.
- **Severity:** Critical.
- **Trigger:** first repository edit after graph extraction.
- **Superior:** content/snapshot-addressed immutable graph artifacts.
- **Migration:** cache key includes snapshot/extractor/config hash.

### A4. Code graph provenance lacks canonical evidence model

- **Current:** external Graphify edge contains confidence/source file/location, while code-intelligence relations are simpler arrays.
- **Severity:** High.
- **Superior:** one typed edge model with extractor/evidence/time.

### A5. Memory temporal model conflates lineage with time

- **Current:** created/updated/TTL/supersession useful but no verified bitemporal validity.
- **Severity:** High.
- **Trigger:** historical queries and delayed event ingestion.
- **Superior:** `valid_from/to` + `recorded_from/to` or append-only versions.

### A6. Retrieval architecture lacks demonstrated hybrid rigor

- **Current:** threshold/recency/access signals; code query matching includes direct string logic.
- **Severity:** High.
- **Superior:** retriever ensemble + score calibration + RRF + reranker + trace.

### A7. Service granularity exceeds proven scale requirement

- **Current:** many independently deployed services.
- **Severity:** Medium/High operationally.
- **Trigger:** release coordination, schema upgrades, constrained hardware.
- **Superior:** modular monolith/worker set for local/default; separable services for enterprise scale.

### A8. Deployment parity has already failed

- **Current evidence:** repo history records production fabric stale relative to repository feature batches.
- **Severity:** High.
- **Superior:** signed release manifest, image SHA, source SHA, schema version and canary parity checks.

### A9. Benchmarking is not the source of architectural decisions yet

- **Current:** harness exists, but no demonstrated market benchmark discipline.
- **Severity:** High strategic.
- **Superior:** every retrieval/index change runs benchmark gates and ablations.

### A10. No explicit public license observed in repository metadata

- **Severity:** High adoption/legal blocker.
- **Fix:** choose and commit a license before encouraging external adoption/contribution.

---

## 22. Recommended Target Architecture

### 22.1 Textual architecture diagram

```text
                         ┌──────────────────────────────┐
                         │ Agents / SDKs / MCP / CLI/UI │
                         └──────────────┬───────────────┘
                                        │
                         ┌──────────────▼───────────────┐
                         │      SCOS Knowledge API      │
                         │ auth • tenancy • policy      │
                         └───────┬───────────┬──────────┘
                                 │           │
                  ┌──────────────▼───┐   ┌──▼────────────────┐
                  │ Context Planner   │   │ Ingestion Gateway │
                  │ query classifier │   │ events/repos/git   │
                  └──────┬───────────┘   └──┬────────────────┘
                         │                  │
          ┌──────────────▼──────────────────▼─────────────────┐
          │              Unified Evidence Plane              │
          │ episode • claim • entity • decision • source     │
          │ repo • commit • file • symbol • typed edge       │
          │ bitemporal validity • provenance • confidence    │
          └───────┬───────────────┬────────────────┬─────────┘
                  │               │                │
       ┌──────────▼─────┐  ┌──────▼───────┐  ┌────▼─────────┐
       │ Canonical DB   │  │ Object Store │  │ Job/Event DB │
       │ Postgres/SQLite│  │ raw/snapshots│  │ checkpoints  │
       └──────┬─────────┘  └──────────────┘  └────┬─────────┘
              │                                    │
     ┌────────▼────────────────────────────────────▼───────────┐
     │                    Projection Workers                  │
     │ memory extraction • SCIP • Tree-sitter • CPG adapters │
     │ embeddings • BM25/trigram • graph • summaries         │
     └───────┬──────────────┬───────────────┬─────────────────┘
             │              │               │
       ┌─────▼─────┐  ┌────▼─────┐   ┌────▼─────────┐
       │ Lexical   │  │ Vector   │   │ Graph Index  │
       │ FTS/Zoekt │  │ pgvector │   │ SQL/Neo4j... │
       └─────┬─────┘  └────┬─────┘   └────┬─────────┘
             └──────────────┼───────────────┘
                            │
                    ┌───────▼────────┐
                    │ Hybrid Retriever│
                    │ RRF • rerank    │
                    │ temporal • ACL  │
                    └───────┬────────┘
                            │
                    ┌───────▼────────┐
                    │ Context Builder │
                    │ budget • graph  │
                    │ citations       │
                    └─────────────────┘
```

### 22.2 Flow: adding a memory

1. authenticate tenant/principal;
2. persist immutable episode/source;
3. return durable event ID quickly;
4. enqueue extraction;
5. extract candidate claims/entities/relationships;
6. resolve identities;
7. compare temporal/contradiction candidates;
8. append claim versions/edges;
9. update embeddings/sparse/graph projections;
10. evaluate consolidation threshold;
11. update observations asynchronously;
12. publish memory-ready event.

### 22.3 Flow: retrieving memories

1. classify query/temporal intent;
2. resolve subject/namespace/security filters;
3. parallel dense + BM25 + graph + temporal searches;
4. normalize/fuse;
5. expand evidence/related claims;
6. rerank;
7. diversity/dedup;
8. apply token budget;
9. return context + evidence + ranking trace.

### 22.4 Flow: indexing a new repository

1. register repo and credential scope;
2. resolve commit and create snapshot job;
3. enumerate files with ignore policy;
4. language detection;
5. run semantic indexer where available;
6. Tree-sitter fallback;
7. extract deterministic symbols/edges;
8. enrich configs/schemas/routes/tests;
9. create syntax-aware chunks;
10. sparse + dense projections;
11. graph projection;
12. validate index invariants;
13. atomically activate snapshot.

### 22.5 Flow: changed file

Use the incremental algorithm from §11; no active snapshot is mutated until the replacement generation passes validation.

### 22.6 Flow: answer code question

query classifier → symbol/lexical/dense/graph retrievers → fusion → relevant subgraph → code snippets + exact ranges → optional history → context builder → cited answer.

### 22.7 Flow: answer code + historical-memory question

start from code symbols/commits **and** semantic historical query; traverse shared evidence graph through decision/task/conversation/PR links; enforce `as_of` semantics; synthesize only after gathering both evidence classes.

---

## 23. Recommended Data Model

Below is the minimum canonical schema. Types are conceptual; PostgreSQL implementation should use UUID/ULID as appropriate, `timestamptz`, JSONB only for extensible attributes, and typed columns for indexed invariants.

### 23.1 `memory`

- `id`
- `tenant_id`
- `namespace_id`
- `subject_id`
- `observer_id nullable`
- `memory_type`
- `canonical_key nullable`
- `current_version_id`
- `importance`
- `retrieval_utility`
- `privacy_class`
- `retention_policy_id`
- `created_at`
- indexes: tenant+subject+type, namespace, canonical key.

### 23.2 `memory_version`

- `id`
- `memory_id`
- `value_text / value_json`
- `normalized_value_hash`
- `epistemic_confidence`
- `valid_from`, `valid_to`
- `recorded_at`, `superseded_at`
- `supersedes_version_id`
- `status` active/ambiguous/invalidated/stale/forgotten
- `extractor_id`
- `source_count`
- immutable except closing validity/status fields.

### 23.3 `entity`

- `id`, tenant, namespace
- `entity_type`
- `canonical_name`
- aliases
- external identifiers
- resolution confidence
- created/updated

### 23.4 `relationship`

- `id`
- source_entity/node ID
- target_entity/node ID
- relation type
- valid/recorded intervals
- confidence class/score
- extractor/version
- evidence set ID

### 23.5 `source`

- `id`
- type: conversation_turn/document/git_commit/pr/issue/file/tool_result/runtime_event
- external locator
- content hash
- occurred_at
- ingested_at
- actor
- immutable raw-object pointer
- deletion state

### 23.6 `episode`

- `id`
- source ID
- session/project/repo context
- participants
- event time interval
- summary optional
- evidence location

### 23.7 `repository`

- `id`, tenant, canonical remote identity
- default branch
- VCS provider
- visibility/security scope
- latest indexed commit
- indexing config hash

### 23.8 `branch`

- `id`, repo ID, name
- head commit ID
- active snapshot ID
- updated time

### 23.9 `commit`

- repo ID + VCS SHA unique
- parent SHAs
- author/committer
- authored/committed time
- message
- tree hash
- PR/issue links

### 23.10 `file`

Conceptual stable file identity:

- `id`, repo ID
- canonical lineage identity
- first/last commit
- current path convenience field

Separate `file_version`:

- snapshot ID
- file ID
- path
- content hash
- AST hash
- language
- size
- blob/object pointer
- status

### 23.11 `symbol`

Stable conceptual identity:

- ID, repo, language
- semantic descriptor
- kind
- canonical FQN
- first/last known commit

`symbol_version`:

- symbol ID, snapshot ID
- file version ID
- range
- signature/type
- semantic hash
- documentation
- visibility
- status

### 23.12 `code_edge`

- ID
- snapshot/scope
- source symbol/node version
- target symbol/node version
- relation
- confidence class/score
- extractor/version
- source location
- evidence IDs
- valid/recorded time
- properties

### 23.13 `embedding`

- owner type/id/version
- model ID + dimensions
- text/content hash
- vector projection locator
- created time
- stale flag

Never silently mix embeddings from incompatible models/dimensions in one index.

### 23.14 `chunk`

- ID
- owner source/file/symbol
- snapshot/version
- chunk type
- text
- token count
- content hash
- line/range
- parent/child IDs

### 23.15 `indexing_job`

- ID, tenant/repo
- target snapshot/commit
- job type
- state
- idempotency key
- priority
- attempts
- checkpoint
- lease owner/expiry
- created/started/heartbeat/completed times
- error class/detail
- metrics JSONB

Indexes: runnable state+priority, lease expiry, repo+target unique idempotency.

---

## 24. Prioritized Engineering Roadmap

### P0 — Foundation / blockers

| # | Title | Problem / implementation | Affected areas | Complexity | Risk | Impact | Prerequisites |
|---:|---|---|---|---|---|---|---|
| 0.1 | Add explicit license | public reuse legally ambiguous | root | XS | Low | High | decision |
| 0.2 | Canonical evidence schema | memory/code have separate incomplete provenance models | new schema + memory/code services | XL | High | Transformational | schema ADR |
| 0.3 | Durable code index store | JSON maps cannot scale | `services/code-intelligence` | L | Medium | Transformational | 0.2 |
| 0.4 | Snapshot/generation model | mutable/stale code views | code intelligence + graphify | L | Medium | Transformational | 0.2/0.3 |
| 0.5 | SCIP ingestion framework | no precise semantic symbols | language adapters | L | Medium | Transformational | 0.3/0.4 |
| 0.6 | Tree-sitter fallback indexer | broad syntax coverage missing | language adapters | L | Medium | High | 0.3 |
| 0.7 | Snapshot-addressed Graphify cache | stale graph currently possible | `graphify-provider` | S/M | Low | High | 0.4 |
| 0.8 | Durable indexing queue/checkpoints | resumability/idempotency not proven | worker/job layer | L | Medium | High | 0.4 |
| 0.9 | Baseline benchmark suite | architecture not gated by quality | `benchmark-harness` | L | Low | Transformational | none |
| 0.10 | Release parity manifest | deployment drift observed | deploy/CI/services | M | Low | High | none |
| 0.11 | Tenant isolation test matrix | cross-service isolation must be proven | all APIs/indexes/cache | M | Medium | High | test harness |
| 0.12 | Derived deletion ledger | privacy deletion not provable across projections | memory/index workers | L | High | High | 0.2 |

**Tests required for every P0:** migration rollback, crash/retry idempotency, cross-tenant negatives, snapshot consistency, checksum reconciliation, corpus rebuild, fault injection.

### P1 — Major competitive gaps

| # | Title | Competitor evidence / implementation | SCOS implementation | Complexity | Impact |
|---:|---|---|---|---|---|
| 1.1 | Bi-temporal claim/edge versions | Graphiti | valid + transaction time on claims/edges | L | Transformational |
| 1.2 | BM25/sparse memory search | Hindsight/Graphiti | Postgres FTS/BM25-like adapter + optional engine | M | High |
| 1.3 | Dense retrieval normalization | all | standardized vector adapter + score calibration | M | High |
| 1.4 | Hybrid RRF fusion | Graphiti/Hindsight | parallel retrievers + RRF trace | M | High |
| 1.5 | Cross-encoder reranker | common production pattern | pluggable rerank stage | M | High |
| 1.6 | Observation/conclusion layer | Hindsight/Honcho | evidence-backed consolidation | L | Transformational |
| 1.7 | Query planner | Graphiti recipes | intent → retriever plan | M | High |
| 1.8 | Precise relation graph | SCIP/Sourcegraph | definitions/references/implementations | L | Transformational |
| 1.9 | Lexical code index | Zoekt | Postgres trigram first, Zoekt adapter later | M | High |
| 1.10 | Query-conditioned repo-map | Aider | personalized graph centrality under token budget | M | High |
| 1.11 | Incremental hash/diff pipeline | Sourcegraph-class indexing | content/AST/symbol hashes + invalidation | XL | Transformational |
| 1.12 | Rename/symbol lineage | precise code history | Git+semantic fingerprint lineage | L | High |
| 1.13 | Test relationship index | code graph tools | static + coverage evidence | L | High |
| 1.14 | Retrieval trace/explain API | Hindsight evidence ethos | score contributions + sources | M | High |
| 1.15 | Embedded local mode | Hindsight/Cognee | SQLite+FTS+embedded vector | L | High |

### P2 — Differentiators

| # | Title | Proposed implementation | Complexity | Impact |
|---:|---|---|---|---|
| 2.1 | Observer→observed beliefs | Honcho-style perspective fields + trust graph | L | High/Transformational for multi-agent |
| 2.2 | Memory blocks / pinned context | bounded editable context packs | M | High DX |
| 2.3 | Decision→code provenance | conversations/decisions/tasks/PRs/commits/symbols | L | Transformational |
| 2.4 | Automatic stale-memory detection | code tombstone/change events invalidate linked claims | L | Transformational |
| 2.5 | Impact outcome learning | train/calibrate from `ImpactReconciliation` | L | High |
| 2.6 | Ontology/schema packs | controlled extraction for code/infra/product domains | M | High enterprise |
| 2.7 | Multi-repo graph | package/service/repository dependency identity | XL | Transformational |
| 2.8 | Graph explorer + timeline | visual evidence/version graph | L | High DX |
| 2.9 | Storage adapter conformance suite | vector/graph/search providers | M | Medium/High |
| 2.10 | Background reasoning budgeter | token/cost threshold scheduler like Honcho principle | M | High |

### P3 — Advanced / moat

| # | Title | Proposed implementation | Complexity | Risk | Impact |
|---:|---|---|---|---|---|
| 3.1 | CPG overlays | CFG/data-flow/static-analysis adapters | XL | High | High |
| 3.2 | Causal change graph | infer/test cause paths from commits/incidents | XL | High | Transformational |
| 3.3 | Learned retrieval policy | contextual bandit/ranker using agent outcome signals | XL | High | Transformational |
| 3.4 | Repository-specific ranking models | learn patterns per repo/org | XL | Medium | High |
| 3.5 | Counterfactual impact simulation | use graph + tests + history to predict breakage | XL | High | Transformational |
| 3.6 | Cross-repo semantic lineage | copied/forked/generated API lineage | XL | High | High |
| 3.7 | Continuous architecture model | automatically maintained subsystem map/ADRs | L/XL | Medium | Transformational |

---

## 25. Moat Opportunities

| Rank | Moat | Defensibility | Practical value | Why |
|---:|---|---|---|---|
| 1 | **Decision-to-code provenance graph** | Very High | Very High | requires owning memory + Git + code graph + evidence |
| 2 | **Automatic stale-memory invalidation from code changes** | Very High | Very High | solves a real failure no generic memory layer can see |
| 3 | **Bitemporal memory + commit-aware code graph** | Very High | Very High | answers “what was true then?” across human and code history |
| 4 | **Deterministic + semantic graph fusion with confidence classes** | High | Very High | trustworthy agent reasoning without pretending all edges equal |
| 5 | **Outcome-trained change-impact prediction** | High | High | proprietary learning signal improves with usage |
| 6 | **Graph-based context assembly under budget** | Medium/High | Very High | directly reduces tokens and wrong-context errors |
| 7 | **Multi-repository dependency intelligence** | High | High | valuable for enterprises/microservices |
| 8 | **Observer-aware multi-agent knowledge** | Medium/High | High for swarms | different agents can hold different beliefs/trust |
| 9 | **Self-improving retrieval from agent outcomes** | High | High | compounds with usage but requires careful causal attribution |
| 10 | **Portable cross-harness memory + code context** | Medium | High | excellent distribution/DX, less algorithmically defensible |

---

## 26. Definition of “Best on the Market”

SCOS should not claim “best” because it has the longest feature table. It earns the claim only when it meets measurable criteria.

### Memory

- top-tier reproducible LongMemEval/LoCoMo performance on openly specified models/configs;
- best-in-class temporal correction and contradiction accuracy;
- evidence precision >98% on internal benchmark;
- deletion verified across canonical + derived stores;
- no material regression when scaling from 10k to 10M memories.

### Code

- >99% definition accuracy on semantically indexed languages;
- reference/implementation quality near language-tool ground truth;
- lexical search latency competitive with dedicated tools at target scale;
- materially better bug localization and impact analysis than dense-only/vector-RAG baselines;
- exact snapshot/commit answers with zero stale-generation leakage.

### Unified intelligence

SCOS must uniquely demonstrate tasks like:

> “Show me the decision that introduced this retry behavior, the conversation evidence, the PR/commit that implemented it, all current callers, the tests that cover it, and whether the original rationale is still valid after last month's refactor.”

The answer must include a traversable evidence chain.

### Developer experience

- useful in under 5 minutes locally;
- one-command coding-agent integration;
- inspectable memory and graph state;
- no mandatory multi-database circus for small users;
- clear migration path to enterprise scale.

### Reliability

- reproducible backups and restore drills;
- idempotent workers;
- no cross-tenant cache/index leakage;
- release/source/schema parity verified automatically;
- documented p95/p99 SLOs under published load profiles.

---

## 27. Final Recommended Build Order

Do this in dependency order. Do not start with UI polish or another clever memory taxonomy.

1. **Freeze and publish baseline benchmarks.** Measure current SCOS before touching architecture.
2. **Define the canonical Unified Evidence Graph schema and temporal semantics.** This is the contract everything else projects from.
3. **Migrate code-intelligence off JSON/in-memory maps.** Keep compatibility import/export only.
4. **Introduce immutable repository/index snapshots.** Make stale generation leakage structurally impossible.
5. **Add SCIP ingestion and Tree-sitter fallback.** Precision before fancy graph inference.
6. **Build durable incremental indexing with hashes, Git diffs, rename lineage, dependency invalidation and checkpoints.**
7. **Replace Graphify existence cache with snapshot-addressed artifacts.** Then decide whether Graphify remains an adapter or is gradually superseded by native projections.
8. **Create canonical typed code edges with deterministic/probabilistic confidence classes and provenance.**
9. **Implement lexical + dense + graph + temporal retrieval adapters and RRF fusion.**
10. **Add query planning, reranking and retrieval traces.** Benchmark every stage with ablations.
11. **Upgrade memory to bitemporal claim versions and evidence-backed observations.** Preserve current supersession/merge mechanics during migration.
12. **Build unified decision/conversation/task/PR/commit/symbol links.** This is where SCOS stops being an imitation and becomes its own product.
13. **Implement automatic stale-memory invalidation driven by repository events.**
14. **Turn `ImpactReconciliation` into a real calibrated learning loop.** Use observed builds/tests/runtime outcomes, not LLM vibes.
15. **Add local embedded mode and polished SDK/MCP onboarding.** Make the sophisticated architecture disappear behind a simple interface.
16. **Add observer→observed representations for multi-agent deployments.** Only after core truth/evidence semantics are stable.
17. **Add deeper CPG/static-analysis overlays where benchmarks prove value.** Never build CFG/data-flow merely to put it on a landing page.
18. **Publish reproducible competitive results and raw methodology.** If SCOS is better, make the evidence boringly undeniable.

---

# Appendix A — Evidence Register

## SCOS sources inspected

- `https://github.com/dylmarriner/scos-memory` at commit `78755c40b2ba36300313da1ba5c5df029ff4aab6`
- `services/memory-intelligence/cmd/server/main.go` — memory turn handling, thresholds, duplicate/supersede review, retrieval parameters.
- `services/memory-intelligence/cmd/server/store.go` — fact schema, PostgreSQL writes/reads, supersession, forgetting, merge logging, access tracking.
- `services/code-intelligence/cmd/server/main.go` — repository/symbol/chunk/generation models, in-memory/JSON persistence, query/index/impact APIs, language-specific extraction.
- `services/code-intelligence/cmd/server/main_test.go` — test surface exists; full test quality was not individually enumerated in this pass.
- `services/graphify-provider/cmd/server/graphify.go` — CLI execution, graph caching, OOM constraints, graph schema, confidence/source metadata, explicit stale-cache limitation.
- `packages/database/*` — DB pool/TLS helper package surface.
- repository commit history — deployment/source drift and auto-deploy canary changes.

## Competitor sources

### Hindsight

- `https://github.com/vectorize-io/hindsight`
- README and developer API/best-practices docs inspected 2026-09-01.
- `hindsight-docs/src/pages/best-practices.mdx`
- `skills/hindsight-docs/references/developer/retain.md`
- `skills/hindsight-docs/references/developer/api/quickstart.md`
- release article `2026-06-08-version-0-8-0.md`

### Honcho

- `https://github.com/plastic-labs/honcho`
- README, reasoning docs, CLAUDE architecture notes and changelog inspected 2026-09-01.

### Mem0

- `https://github.com/mem0ai/mem0`
- `mem0/memory/main.py`
- README benchmark disclosures inspected 2026-09-01.

### Letta

- `https://github.com/letta-ai/letta`
- `letta/schemas/memory.py`
- `https://github.com/letta-ai/letta-code` memory filesystem prompt/docs.

### Graphiti / Zep

- `https://github.com/getzep/graphiti`
- `graphiti_core/edges.py`
- `graphiti_core/utils/maintenance/edge_operations.py`
- `graphiti_core/search/search.py`
- `graphiti_core/search/search_config.py`
- `graphiti_core/search/search_utils.py`
- `https://github.com/getzep/zep` current README/deprecation notice.

### Cognee

- `https://github.com/topoteretes/cognee`
- architecture docs, `.env.template`, examples and provider documentation.

### LangMem

- `https://github.com/langchain-ai/langmem`
- background quickstart and README.

### OpenMemory

- `https://github.com/mem0ai/openmemory`
- README inspected 2026-09-01.

### Code intelligence

- SCIP: `https://github.com/scip-code/scip`, `scip.proto`
- Joern: `https://github.com/joernio/joern`, CPG specification ecosystem
- Aider: public RepoMap implementation/design (`repomap.py` / Tree-sitter + PageRank pattern)
- Sourcegraph precise code-navigation documentation / SCIP ecosystem
- Zoekt public repository/code-search architecture

# Appendix B — Claims That Remain Unknown Until Runtime Reproduction

The following should **not** be promoted to “verified” without a controlled runtime campaign:

- exact Hindsight benchmark scores under SCOS-selected identical model/provider/hardware;
- exact Mem0 managed-platform performance, because the project itself states proprietary hosted optimizations differ from OSS;
- Zep Cloud internals and <200 ms product claims under comparable workloads;
- every competitor's tenant isolation and deletion guarantees under adversarial testing;
- SCOS large-scale throughput, because current code-index architecture was not load-tested in this audit environment;
- Graphify's extraction precision across all languages/repositories;
- code-impact precision until evaluated against compiler/static-analysis and failing-test ground truth.

# Appendix C — Immediate Acceptance Tests for the First Rewrite

A replacement code-index generation is not considered correct until all pass:

1. indexing the same commit twice produces identical canonical hashes;
2. a one-line function-body edit updates only affected file/symbol/chunk projections plus justified dependants;
3. a pure rename preserves symbol lineage;
4. deleting a symbol creates a tombstone and no current-snapshot query returns it;
5. branch A and branch B can contain conflicting symbol versions without leakage;
6. failed indexing never advances the active branch snapshot;
7. worker retry is idempotent;
8. vector model change creates a new embedding generation rather than mixing dimensions/models;
9. graph extractor/config change creates a new graph generation;
10. cross-tenant queries cannot infer existence, counts, IDs or cache artifacts;
11. a code-linked memory becomes stale when its sole referenced symbol disappears;
12. historical `as_of` query still returns the old fact/code state;
13. restoring canonical DB/object storage can rebuild all indexes to identical logical counts/hashes;
14. release health endpoint returns source SHA, schema version, index format version and config fingerprint;
15. benchmark gates fail CI when retrieval or index accuracy regresses beyond defined tolerance.

---

**Bottom line:** SCOS has the right *scope* to become unusually strong, but scope is not the same thing as capability. The memory store has genuine lifecycle engineering; the current code-intelligence core does not yet deserve the name “market-leading.” Fix semantic indexing, temporal evidence, snapshots, retrieval fusion and benchmarks first. Then use the unique intersection of personal/project memory and commit-aware code history to build the feature competitors cannot cheaply bolt on later: a continuously maintained, evidence-backed explanation of not only **what the system is**, but **how and why it became that way**.
