# SCOS / Nexus Persistent Agent Memory Fabric

## Comprehensive Technical Governing Blueprint

**Document version:** 1.3.0  
**Status:** Governing baseline  
**Owner:** Dylan Marriner  
**System:** SCOS Memory Fabric  
**Primary implementation:** Nexus Memory  
**Adopted:** 26 July 2026

---

## 1. Executive ruling

SCOS Memory Fabric SHALL provide one governed, model-neutral memory plane for authorised AI agents, applications, command-line tools and orchestration systems.

The system SHALL:

1. Durably record every accepted user message, completed tool operation, agent reply and state transition.
2. Recover active conversations and tasks after process crashes, network failures, operating-system failures and power loss.
3. Separate complete conversation history from selectively promoted long-term knowledge.
4. Provide hybrid lexical, semantic, relational, metadata and temporal retrieval.
5. Support private, agent, project, workspace, organisation and public-reference scopes.
6. Preserve provenance, evidence, authority, versions, corrections and revocations.
7. Prevent agents from directly mutating authoritative shared knowledge.
8. Expose memory through HTTP, SDK, MCP and optional A2A adapters.
9. Store curated portable knowledge in OKF-compatible Git repositories.
10. Treat embeddings, search indexes, caches and materialised views as rebuildable derivatives.
11. Optimise input and output tokens through a native, reversible context-optimisation layer without sacrificing instructions, evidence, precision or recoverability.
12. Provide a native repository-scale code-intelligence layer combining exact, symbolic, structural, relational, semantic and historical understanding.
13. Route commands and tool results through a native adaptive execution proxy that compresses noise, preserves diagnostics, learns from outcomes and evolves only through governed evaluation.
14. Operate through an always-on native Autonomous Cognitive Control Plane that intercepts every interaction, automatically selects and coordinates required systems and tools, hydrates context before agent execution, persists state after execution and never depends on an agent remembering to call memory.
15. Provide a native package system for installable plugins, skills, hooks and agent adapters with declarative permissions, signatures, compatibility checks, isolation and rollback.
16. Integrate with every agent runtime that exposes at least one supported boundary, including native SDK, HTTP, MCP, model proxy, command wrapper, event stream, session hook or provider interface.
17. Operate as a first-class Hermes Agent memory provider through the complete Hermes memory lifecycle without requiring the model to remember to invoke memory.
18. Provide native retain, recall, reflect, entity modelling, temporal graph, belief revision, diary, hierarchy and background-capture capabilities at least equivalent to leading agent-memory systems.
19. Expose a real-time transparency dashboard showing authorised operational state, memory links, facts, learnings, user representations, agents, tools, plugins, skills, hooks, routing, token flow, code indexes and recovery activity.

> **Intercept every interaction. Persist every turn. Route every capability. Promote only durable knowledge.**

Every completed turn SHALL be recoverable. Every statement SHALL NOT automatically become trusted long-term memory. Those are separate jobs, despite the industry habit of throwing both into a vector database and hoping confidence emerges from the fog.

---

## 2. Normative language

- **MUST / SHALL:** mandatory.
- **MUST NOT / SHALL NOT:** prohibited.
- **SHOULD:** expected unless a documented exception exists.
- **SHOULD NOT:** normally prohibited unless a documented exception exists.
- **MAY:** optional.

An implementation claiming compliance MUST satisfy every mandatory requirement in this document.

---

## 3. Objectives

### 3.1 Primary objectives

The platform SHALL provide:

- Crash-safe conversation continuity.
- Persistent task and workflow state.
- Reliable multi-agent handoffs.
- Durable user, project and system knowledge.
- Auditable memory changes.
- Portable curated knowledge.
- Explainable retrieval.
- Controlled correction, expiry, revocation and deletion.
- Framework-independent integration.
- Recovery independent of the model that created the original state.
- Model-neutral input and output token optimisation.
- Minimal-context code understanding across large repositories.
- Reversible command and tool-output reduction.
- Bounded self-improvement backed by measurable outcomes and safe rollback.
- Automatic pre-turn retrieval, context compilation, tool routing, response persistence, checkpointing and post-turn consolidation.
- Agent-independent operation: memory and orchestration SHALL work even when the selected agent has no memory awareness or memory tools in its prompt.
- Automatic selection of the correct internal system, capability or authorised external connector based on intent, scope, risk, freshness, cost and observed performance.
- Installable native plugins, portable skills, deterministic hooks and agent adapters.
- Practical compatibility across heterogeneous agent frameworks and runtimes.
- First-class Hermes Agent memory-provider operation.
- Temporal, entity-aware retain, recall and reflection.
- A complete, correctable and evidence-backed model of authorised user knowledge.
- Real-time operational transparency without exposing secrets or private model chain-of-thought.
- Comparative performance that must exceed defined external and internal baselines before any superiority claim is permitted.

### 3.2 Initial recovery objectives

| Scenario | Recovery Point Objective | Recovery Time Objective |
|---|---:|---:|
| Process crash | Zero loss of committed turns | Under 2 minutes |
| Host reboot or power failure | Zero loss of committed turns | Under 10 minutes |
| Interrupted streamed reply | No more than one configured chunk interval | Under 2 minutes |
| Primary database failure with standby | Near-zero committed-event loss | Under 5 minutes |
| Complete host loss | No more than the WAL archive interval | Under 60 minutes |
| Search-index corruption | No authoritative data loss | Rebuild without source loss |

A turn is protected only after its authoritative transaction commits. Anything else is optimism wearing a status badge.

### 3.3 Non-goals

The first implementation SHALL NOT:

- Store or expose hidden model reasoning.
- Promote every model inference.
- Build a graph node for every sentence.
- Allow autonomous rewriting of approved governance.
- Replace source repositories, databases or file systems.
- Claim exactly-once external effects where providers lack idempotency or reconciliation.
- Depend on one model, embedding provider or agent framework.
- Use an agent's voluntary tool choice as the only trigger for memory, retrieval, indexing, checkpointing or consolidation.
- Let a probabilistic model be the sole authority for high-risk routing or permission decisions.
- Introduce Kafka, Kubernetes or a dedicated graph database without measured need.

---

## 4. Governing principles

### 4.1 Authoritative data is not an embedding

Authoritative records are:

- Append-only events.
- Original messages and response chunks.
- Tool-operation records.
- Checkpoints.
- Memory records and versions.
- Evidence and source references.
- Approved OKF documents and Git history.
- Access policies and audit records.

Embeddings, lexical indexes, caches and ranking outputs SHALL be disposable and reproducible.

### 4.2 Journal first

Every accepted state-changing operation SHALL produce a durable journal event.

### 4.3 Commit before completion

A completed agent reply SHALL be committed before the server signals completion to a client. Streamed output SHALL be checkpointed at bounded intervals.

### 4.4 Agents are untrusted writers

Agents MAY submit candidates. They SHALL NOT directly:

- Promote memories into protected shared scopes.
- Delete authoritative history.
- Rewrite approved OKF concepts.
- Increase their own authority.
- Mark unsupported inference as verified.

### 4.5 Provenance is mandatory

Every durable memory SHALL identify:

- Source and supporting evidence.
- Creator and approving principal.
- Authority and confidence.
- Validity and observation time.
- Sensitivity and scope.
- Status, version and supersession chain.

### 4.6 Correction, not silent mutation

Incorrect memory SHALL be corrected, superseded or revoked. Historical versions SHALL remain auditable unless lawful deletion requires physical removal.

### 4.7 Scope before relevance

Authorisation SHALL be applied before retrieval candidates are returned or ranked. Permission filtering after semantic retrieval is prohibited.

### 4.8 Simple infrastructure first

The initial production baseline SHALL use PostgreSQL transactions and a transactional outbox. Separate brokers and databases require an ADR and measured justification.

### 4.9 Quality before compression

Token reduction SHALL remain subordinate to instruction fidelity, evidence integrity, code correctness, error visibility, safety and task completion. Any optimisation that cannot prove preservation of required meaning SHALL fall back to the authoritative unoptimised form.

### 4.10 Reversible optimisation

Every lossy summary, context slice, filtered command result or compressed artifact SHALL retain stable references to its authoritative source. Authorised agents and operators SHALL be able to restore the exact original material.

### 4.11 Native ownership

The three optimisation systems defined by this document SHALL be native SCOS components owned in this repository. Their core operation SHALL NOT depend on an external hosted service or on a third-party runtime product. Optional adapters MAY exist, but the platform SHALL remain fully functional without them.

### 4.12 Bounded evolution

Self-learning components MAY select among approved policies and MAY propose new rules, parsers, ranking weights, budgets and transformations. They SHALL NOT silently rewrite governing constraints, expand permissions, deploy unreviewed executable code or alter protected policies. Learned changes SHALL pass replay, regression, adversarial, canary and rollback gates.

### 4.13 Measured superiority

The platform SHALL pursue category-leading performance, but superiority SHALL be demonstrated rather than declared. A subsystem MAY be labelled superior only after reproducible benchmarks show meaningful improvement across quality, token use, latency, reliability, safety and recovery against current relevant baselines. Winning one favourable metric while degrading correctness SHALL count as failure.

### 4.14 Platform-owned cognition lifecycle

The platform, not the selected agent, SHALL own message ingestion, memory retrieval, context assembly, capability routing, tool mediation, persistence, checkpointing, consolidation, indexing and recovery. Agents SHALL receive prepared context and authorised capabilities. Their awareness of the memory system SHALL be optional.

### 4.15 Automatic by default, explicit at boundaries

Read-only retrieval, context optimisation, journalling, indexing, health checks, deduplication and post-turn memory analysis SHALL occur automatically. Human approval SHALL still be required where governing policy requires confirmation, including destructive, financial, legal, permission-changing, external-communication and other high-impact actions.

### 4.16 Deterministic constraints before learned ranking

Routing SHALL first enforce identity, authority, data classification, side-effect, validity, freshness and health constraints deterministically. Learned models MAY rank eligible options only after prohibited and unavailable options have been removed.

### 4.17 Explainable autonomy

Every autonomous decision SHALL be attributable to a versioned policy, capability registry state, evidence set, confidence score and reason codes. Operators SHALL be able to replay the decision against the same inputs.

### 4.18 Extensions are governed code

Plugins, skills, hooks and adapters SHALL be treated as executable supply-chain components. Convenience SHALL NOT bypass identity, permission, signature, compatibility, audit, isolation, revocation or rollback requirements.

### 4.19 Universal compatibility means supported boundaries

The system SHALL support every agent runtime that exposes at least one documented integration boundary. Closed systems that expose no plugin, hook, API, proxy, command, event or session boundary cannot be truthfully integrated and SHALL be reported as unsupported rather than quietly simulated.

### 4.20 Transparency is operational, not private reasoning disclosure

The platform SHALL expose all authorised inputs, outputs, routes, retrieved memories, evidence, transformations, tool operations, policies, confidence, costs, timing and state changes. It SHALL NOT claim to expose private model chain-of-thought. Decision summaries, reason codes and evidence SHALL provide auditable transparency without fabricating hidden reasoning.

### 4.21 User knowledge is inspectable and correctable

Every authorised fact, preference, goal, relationship, pattern, inference, contradiction and learning associated with a user SHALL be discoverable, attributable, versioned and correctable. Secret values and inaccessible third-party data SHALL remain protected.

---

## 5. Standards baseline

- **OKF-compatible Markdown/YAML:** governing knowledge, decisions, procedures, architecture and stable concepts.
- **CloudEvents-compatible envelope:** internal events.
- **JSON Schema Draft 2020-12:** events, API payloads, handoffs and memory structures.
- **OpenAPI 3.1+:** HTTP contracts.
- **MCP:** agent access to tools and resources.
- **A2A:** optional peer-agent discovery and task delegation.
- **OpenTelemetry:** traces, metrics and logs.
- **SCOS Extension Manifest:** signed declarative plugin, skill, hook and adapter packages.
- **Server-Sent Events and WebSocket:** authorised real-time dashboard delivery.
- **Content Security Policy and Subresource Integrity:** dashboard and extension front-end protection.
- **Capability negotiation:** runtime discovery of agent, provider, hook, skill and protocol support.

Protocol adapters SHALL wrap the Autonomous Cognitive Control Plane and Memory Gateway. They SHALL NOT become persistence authorities.

---

## 6. Logical architecture

```text
Humans / Apps / CLI / IDE / Automations
                  │
      Real-Time Transparency Dashboard
 live topology · traces · user model · memory graph
                  │
      Autonomous Cognitive Control Plane
 ingress · classification · planning · policy
 capability routing · lifecycle · recovery
                  │
     Native Extension and Compatibility Plane
 plugins · skills · hooks · adapters · Hermes provider
      ┌───────────┼────────────┐
      │           │            │
Context       Memory        Code
Optimisation Intelligence  Intelligence
              retain · recall · reflect
      │           │            │
      └───────────┼────────────┘
                  │
        Agents and Orchestrators
                  │
      Autonomous tool-loop interception
                  │
       Adaptive Execution Proxy
                  │
         Authorised tools/connectors
                  │
      commit · checkpoint · consolidate
                  │
             PostgreSQL
 events · messages · checkpoints · memories · beliefs
 plugins · skills · hooks · adapters · user entities
 routing decisions · capability registry · dashboard views
 optimisation runs · policies · index metadata · outbox
      ┌───────────┼────────────┐
      │           │            │
 Object store   OKF/Git      Native indexes
 raw outputs    approved     symbols · graph
 artifacts      knowledge    relations · history
```

---

## 7. Component responsibilities

### 7.1 Memory Gateway

The Memory Gateway SHALL be the only supported application entry point for persistent memory operations. It SHALL provide authentication, policy enforcement, schema validation, idempotency, turn management, retrieval, candidate submission, correction, supersession, OKF sync, auditing and rate controls.

Agents SHALL NOT receive direct database credentials.

### 7.2 Turn Coordinator

The Turn Coordinator SHALL:

- Allocate session, turn, message and event IDs.
- Track lifecycle state.
- Persist messages and streamed chunks.
- Record retrieval and tool-operation references.
- Commit completed replies and checkpoints.
- Enqueue follow-up jobs transactionally.
- Detect and reconcile interrupted work.

### 7.3 Retrieval Orchestrator

The Retrieval Orchestrator SHALL:

- Resolve identity, namespace and permissions.
- Parse exact identifiers, entities and time constraints.
- Search exact metadata, full text, vectors and relations.
- Fuse, deduplicate and rerank candidates.
- Enforce token and sensitivity budgets.
- Return provenance and conflict information.
- Record retrieval diagnostics.

### 7.4 Consolidation Worker

The Consolidation Worker SHALL:

- Extract candidate facts, decisions, preferences, procedures and outcomes.
- Associate evidence.
- Detect duplicates and contradictions.
- Apply authority, confidence, sensitivity and retention rules.
- Promote, merge, quarantine, reject or request review.
- Create embeddings only after classification.
- Produce reviewable OKF proposals.

### 7.5 OKF Synchroniser

The OKF Synchroniser SHALL validate bundles, resolve links, track Git commits, index approved concepts, export approved knowledge, detect divergence and refuse silent overwrites.

### 7.6 Object store

Object storage SHALL hold uploaded files, generated artifacts, large tool outputs, media, exports and backup material. PostgreSQL SHALL retain immutable object references, hashes, metadata and policies.

### 7.7 Native Context Optimisation Layer

The Native Context Optimisation Layer SHALL compile model input, enforce token budgets, create reversible context capsules, preserve instruction hierarchy, optimise cache reuse, validate semantic invariants and compact model output without removing required content.

### 7.8 Native Code Intelligence Layer

The Native Code Intelligence Layer SHALL discover repositories, incrementally parse source, build exact and symbolic indexes, maintain structural and relational knowledge, retrieve minimal sufficient code context and record index lineage. Source repositories SHALL remain authoritative and all indexes SHALL be rebuildable.

### 7.9 Native Adaptive Execution Proxy

The Native Adaptive Execution Proxy SHALL intercept eligible commands and tool results, apply deterministic allowlisted transformations, reduce repetitive output, preserve exit status and diagnostics, store exact raw results, expose restoration handles and collect outcome telemetry for governed improvement.

### 7.10 Autonomous Cognitive Control Plane

The Autonomous Cognitive Control Plane SHALL be the mandatory ingress and orchestration layer for all agent interactions. It SHALL journal input before dispatch, resolve identity and scope, classify intent and risk, select required systems and capabilities, hydrate context, mediate the tool loop, commit replies and checkpoints, trigger consolidation and indexing, record route decisions and recover interrupted orchestration.

No supported agent, model endpoint, tool adapter or client SHALL bypass this control plane for normal operation.

### 7.11 Native Extension Manager

The Native Extension Manager SHALL install, validate, activate, isolate, update, disable, revoke and uninstall plugins, skills, hooks and agent adapters. It SHALL enforce manifests, signatures, compatibility, permissions, dependency locks, health checks and rollback.

### 7.12 Universal Agent Compatibility Gateway

The Universal Agent Compatibility Gateway SHALL negotiate supported boundaries and expose the same governed memory lifecycle through native SDKs, HTTP, MCP, model API proxying, CLI wrappers, provider interfaces, event hooks and session adapters.

### 7.13 Memory Intelligence Engine

The Memory Intelligence Engine SHALL provide retain, recall, reflect, entity modelling, temporal relationships, explicit and inferred fact separation, belief revision, structured user representation, diary generation, hierarchical memory organisation and contradiction handling.

### 7.14 Transparency Projection Service

The Transparency Projection Service SHALL create permission-filtered, real-time read models for the dashboard. It SHALL expose actual authoritative and derived state with provenance and timestamps and SHALL never substitute fabricated demonstration data.

---

## 8. Memory taxonomy

### 8.1 Conversation journal

Complete durable interaction record: messages, tool calls, results, retrievals, checkpoints and failures. Journal content is not automatically trusted knowledge.

### 8.2 Working memory

Short-lived active state: objective, plan, selected artifacts, variables, constraints, pending operations and blockers. Working memory SHALL have expiry or a session boundary.

### 8.3 Episodic memory

What happened, when and in what context: successful deployments, failed commands, rejected options, task completion and observed outcomes.

### 8.4 Semantic memory

Durable facts supported by evidence: project technologies, device identity, infrastructure facts and stable user preferences.

### 8.5 Procedural memory

Reusable methods: runbooks, deployment procedures, troubleshooting sequences, validation steps and agent operating procedures.

### 8.6 Curated governing knowledge

Approved architecture, policy, decisions, contracts and stable domain truth. This layer SHALL be Git-backed and OKF-compatible.

### 8.7 Artifact memory

References to documents, source files, images, reports, logs and generated packages.

### 8.8 Negative and revocation memory

Explicitly disproven, deprecated, revoked, reversed or unsafe knowledge. Negative records SHALL participate in retrieval.

---

## 9. Memory scopes

Required namespace classes:

```text
user/{user_id}/private
agent/{agent_id}/private
project/{project_id}
workspace/{workspace_id}
organisation/{organisation_id}
system/governance
public/reference
```

Every memory SHALL include tenant, namespace, owner, visibility, read/write/promotion policy, retention policy and sensitivity class.

Global shared memory SHALL be exceptional rather than default.

---

## 10. Authority hierarchy

Default conflict precedence:

1. Current explicit human instruction.
2. Approved system policy or governing decision.
3. Human-approved OKF concept.
4. Verified primary source or current system-of-record query.
5. Confirmed tool execution.
6. Repeated independent observation.
7. Single observation.
8. Agent inference.
9. Unsupported model candidate.

Authority determines conflict handling, not automatic truth. Every memory SHALL carry authority, confidence, evidence count, verification status and validity dates.

---

## 11. Turn durability protocol

### 11.1 User message acceptance

A user message SHALL be acknowledged only after one transaction commits the message, event, turn state and session cursor.

### 11.2 Context retrieval

Retrieval requests SHALL include actor, agent, session, turn, namespace scopes, query, memory classes, token budget, sensitivity ceiling and temporal constraints.

### 11.3 Tool execution

Before a side-effecting tool runs, the system SHALL persist a prepared operation containing tool identity, version, input hash, idempotency key, expected effects, retry policy and reconciliation method.

Terminal states:

```text
succeeded
failed
cancelled
unknown
```

Unknown operations SHALL NOT be blindly retried.

### 11.4 Streaming replies

Lifecycle:

```text
assistant_response_started
assistant_response_chunk_committed
assistant_response_chunk_committed
assistant_response_completed
checkpoint_committed
```

Chunks SHALL be committed according to the strictest configured limit: every sentence, every second, every 2-4 KiB or before exceeding the permitted client-visible loss window.

### 11.5 Reply completion transaction

The final reply, turn state, event, checkpoint and outbox job SHALL commit atomically. Completion SHALL be signalled only after commit.

### 11.6 Interrupted generation

Interrupted turns SHALL preserve committed chunks, sequence position, model metadata, context references, completed tools, open operations and a recovery recommendation.

---

## 12. Event model

Every event SHALL contain a unique ID, source, type, subject, timestamp, schema identifier, tenant, correlation ID, causation ID, actor and validated data payload.

Events SHALL be append-only. Corrections SHALL be represented by new correcting, redacting, superseding or reconciliation events.

Every aggregate SHALL maintain a monotonically increasing sequence. The database SHALL enforce uniqueness over `aggregate_id + sequence_number`.

Schema changes SHALL be additive where possible. Removed fields or changed meanings require a new major event type.

---

## 13. Persistence model

Required core tables:

```text
tenants
principals
agents
projects
workspaces
namespaces
namespace_memberships
sessions
turns
messages
message_chunks
events
aggregate_sequences
checkpoints
tool_executions
operations
idempotency_keys
outbox_jobs
dead_letter_jobs
memory_items
memory_versions
memory_relations
memory_evidence
memory_embeddings
memory_candidates
memory_reviews
entities
entity_aliases
entity_representations
beliefs
belief_evidence
diary_entries
plugin_packages
plugin_installations
skills
skill_executions
hooks
hook_subscriptions
hook_executions
agent_adapters
adapter_instances
capability_graph
transparency_projections
dashboard_stream_cursors
okf_bundles
okf_concepts
object_references
retrieval_runs
retrieval_results
retention_rules
audit_entries
schema_versions
```

Stable memory identity SHALL be separated from versioned content. Embeddings SHALL be separated from authoritative content so multiple models and full reindexing remain possible.

Every promoted semantic or governing memory SHALL reference evidence unless explicitly classified as an unsupported hypothesis.

Relations SHALL begin in PostgreSQL. A dedicated graph store requires demonstrated graph-query pressure and an ADR.

---

## 14. Transactional outbox

Every durable change requiring follow-up processing SHALL write its outbox job in the same transaction.

Workers SHALL:

1. Claim jobs with row locking.
2. Use stable operation IDs.
3. Execute idempotently.
4. Commit success state.
5. Retry only retryable failures.
6. Move poison work to a dead-letter queue.
7. Expose failures operationally.

A worker crash after an external side effect but before completion recording SHALL trigger reconciliation rather than blind repetition.

---

## 15. Idempotency

Every write command SHALL accept an idempotency key.

- Identical retries SHALL return the original result.
- The same key with altered input SHALL be rejected.
- Tool calls SHALL use deterministic operation IDs.
- Provider idempotency SHALL be used where available.
- Duplicate event delivery SHALL not duplicate state changes.

---

## 16. Memory consolidation policy

Candidate extraction SHALL run after completed replies, tasks, corrections, decisions, reusable failures, handoffs and imports.

Candidate classes:

```text
discard
journal_only
working
episodic
semantic
procedural
governing
artifact_reference
revocation
```

Promotion requires namespace, source, type, authority, confidence, sensitivity, retention, content hash and conflict assessment.

Human approval SHALL be required for governing policy, security changes, permission expansion, destructive procedures, sensitive inferred traits, organisation-wide promotion and high-impact financial, legal, messaging or production procedures.

Duplicates SHALL merge evidence rather than produce endless paraphrases. Contradictions SHALL create visible conflict sets and SHALL NOT be silently resolved by vector score.

The system SHALL support expiry, sliding expiry, revocation, supersession, confidence decay, archive-only status, lawful deletion and source withdrawal.

---

## 17. Retrieval design

Pipeline:

```text
Authorise
  ↓
Resolve namespaces
  ↓
Parse query, entities and time
  ↓
Apply validity and sensitivity filters
  ↓
Exact + metadata search
  ↓
Full-text search
  ↓
Vector search
  ↓
Relation expansion
  ↓
Recent task/session state
  ↓
Fusion, deduplication and reranking
  ↓
Token budget
  ↓
Content with evidence and conflicts
```

Initial fusion SHALL use Reciprocal Rank Fusion. Cross-encoders or LLM rerankers MAY be added only after a labelled evaluation set proves measurable improvement.

Ranking SHOULD consider lexical and semantic relevance, authority, source quality, recency, validity, entity match, project proximity, relationship distance, task continuity, staleness, conflict and duplication.

Context SHALL be assembled into separate sections for governance, active task state, confirmed facts, prior episodes, procedures, conflicts and source references.

Every retrieval SHALL be explainable and SHOULD be recorded with query hash, scopes, filters, candidate counts, rank configuration, selected IDs, latency, token estimate and model versions.

---

## 18. Native Context Optimisation Layer

### 18.1 Governing purpose

SCOS SHALL include a native, model-neutral Context Optimisation Layer that minimises both input and output token consumption while preserving required meaning, authority, evidence, exact values, code semantics, recoverability and user intent.

The system SHALL optimise the entire token lifecycle rather than merely summarising old conversation text:

```text
authoritative inputs
    ↓
trust and authority classification
    ↓
semantic segmentation and dependency mapping
    ↓
lossless normalisation and deduplication
    ↓
relevance allocation and progressive disclosure
    ↓
reversible compression and source handles
    ↓
model-aware packing and cache planning
    ↓
model invocation
    ↓
output contract validation
    ↓
concise response with restoration references
```

Optimised prompts, summaries, context packs and compact responses SHALL be derived views. Original messages, source files, evidence, policies and raw tool results SHALL remain authoritative.

### 18.2 Input segment classes

Every input segment SHALL be classified before optimisation:

```text
governing_policy
current_user_instruction
active_task_state
acceptance_criteria
confirmed_memory
evidence
code_symbol
code_dependency
prior_episode
tool_schema
tool_result
untrusted_document
conversation_history
optional_background
```

The following SHALL be non-droppable unless explicitly superseded:

- Current user requirements, restrictions and requested output format.
- Governing, security and permission policy.
- Acceptance criteria and unresolved constraints.
- Exact identifiers, paths, versions, values, commands and error text needed by the task.
- Evidence needed to support factual conclusions.
- Relevant errors, warnings, failed assertions and contradictory information.
- Recovery, operation and idempotency identifiers.
- Source attribution required for audit or restoration.

### 18.3 Input optimisation pipeline

The layer SHALL support these ordered transformations:

1. **Canonicalisation:** remove transport noise and normalise equivalent formatting without changing meaning.
2. **Exact deduplication:** retain one authoritative copy of byte-identical content and replace repetitions with references.
3. **Semantic deduplication:** merge propositions only when meaning, scope, validity, authority and evidence align.
4. **Delta context:** send changes relative to the last acknowledged context snapshot rather than repeatedly sending unchanged state.
5. **Instruction factoring:** keep one canonical policy version and reference inherited policy by stable identifier and hash.
6. **Task-state compilation:** transform long history into structured objective, decisions, constraints, completed work, blockers and next actions while preserving source references.
7. **Progressive disclosure:** initially supply the smallest sufficient view and expose deterministic expansion handles.
8. **Relevance allocation:** assign tokens using authority, task relevance, evidence need, uncertainty, expected value and dependency distance.
9. **Code-aware slicing:** retrieve complete symbols, types, contracts, tests and dependencies instead of arbitrary character windows.
10. **Tool-schema routing:** include only task-relevant tool definitions.
11. **Cache-aware ordering:** place stable reusable material before volatile material and identify stable content by hash.
12. **Tokenizer-aware accounting:** use the selected model's tokenizer when available and a conservative model-neutral estimate otherwise.
13. **Optional local semantic compression:** use a locally controlled model only after deterministic methods are exhausted and quality gates pass.
14. **Invariant validation:** prove required instructions, evidence, exact values and constraints survived compilation.

### 18.4 Compression profiles

Required profiles:

| Profile | Allowed behaviour | Intended use |
|---|---|---|
| `exact` | Canonicalisation, exact deduplication and references only | Governance, legal, security and fragile work |
| `conservative` | Lossless transforms plus low-risk structured compaction | Default unfamiliar work |
| `balanced` | Reversible semantic compression with validation | Routine agent and coding work |
| `aggressive` | Strong reduction with progressive retrieval and strict post-checks | Oversized histories, logs and broad repositories |
| `adaptive` | Chooses approved policy per segment | Default after evaluation proves safe |

High-impact or restricted tasks SHALL default to `exact` or `conservative`.

### 18.5 Reversible context capsules

Every lossy or selective context view SHALL produce a capsule containing:

```text
capsule_id
source_object_ids
source_content_hashes
source_versions
included_ranges
excluded_ranges
compression_profile
compiler_version
policy_version
token_accounting_method
original_token_count
compiled_token_count
semantic_invariants
validation_results
restoration_handles
created_at
```

Capsules SHALL be immutable and auditable. A capsule SHALL never replace its source.

### 18.6 Context budget allocator

The allocator SHALL reserve capacity before optional material is considered. A default allocation SHOULD include:

```text
15% governing policy and safety
15% current request and output contract
15% active task state and acceptance criteria
25% evidence and relevant memory
20% code, tools and artifacts
10% generation reserve and contingency
```

Actual allocation MAY change by task, but mandatory classes SHALL not be displaced by optional context. Unused reserve MAY be reassigned dynamically.

The allocator SHALL optimise for expected task utility per token, not similarity alone. It SHALL account for:

- Authority.
- Direct relevance.
- Dependency centrality.
- Evidence value.
- Recency and validity.
- Conflict risk.
- Cost of later retrieval.
- Consequence of omission.
- Model context and output limits.

### 18.7 Output token optimisation

Output optimisation SHALL occur through an explicit response contract, not blind truncation.

The system SHALL:

- Determine required answer depth, format and artifact type before generation.
- Prevent repeated conclusions, duplicated caveats and redundant restatement of the request.
- Use references for unchanged context rather than reproducing it.
- Separate full durable artifacts from concise chat summaries.
- Prefer structured diffs and patches over complete unchanged files.
- Prefer targeted diagnostics over raw repeated logs.
- Stop generation when acceptance criteria are satisfied.
- Preserve errors, uncertainty, safety information and necessary reasoning summaries.
- Allow expansion of any compressed section through stable references.

The optimiser SHALL NOT shorten content merely to improve a token metric when the user asked for comprehensive detail or when omission could alter a decision.

### 18.8 Adaptive policy selection

The layer MAY learn which approved profile and budget work best for a task class using outcome telemetry. Learning inputs MAY include:

```text
task type
model
context composition
original and compiled tokens
latency
cost
retrieval expansions
user corrections
answer acceptance
unit and integration test results
citation and invariant failures
```

Learning SHALL use bounded policy selection. It SHALL NOT rewrite governing rules. Candidate policy changes SHALL be evaluated offline and deployed through the change process.

### 18.9 Token optimisation metrics

Required metrics:

```text
input_token_reduction
output_token_reduction
total_token_reduction
semantic_retention
instruction_retention
exact_value_retention
evidence_retention
answer_quality_delta
task_success_delta
expansion_rate
fallback_rate
cache_reuse_rate
compression_latency
cost_per_successful_task
```

### 18.10 Superiority gate

The Context Optimisation Layer SHALL not be considered superior until it demonstrates, across a representative benchmark suite:

- At least 50% median total-token reduction against an unoptimised baseline.
- At least 30% reduction against the strongest tested competing approach.
- No statistically meaningful degradation in task success, factual accuracy, code correctness or citation quality.
- At least 99.5% preservation of mandatory instructions and exact task-critical values.
- 100% restoration success for referenced source material.
- Lower cost per successful task, not merely lower raw token count.
- p95 compilation latency within the approved interactive budget.

Targets MAY be tightened after the first benchmark generation. They SHALL NOT be weakened to manufacture a win.

---

## 19. Native Code Intelligence Layer

### 19.1 Governing purpose

SCOS SHALL include a native Code Intelligence Layer designed to provide precise repository-scale understanding while sending the smallest sufficient code context to agents.

It SHALL treat source control content as authority and maintain disposable, versioned indexes. It SHALL support monorepositories, multiple repositories, branches, worktrees, generated code, vendored code, ignored paths and partial checkouts.

### 19.2 Intelligence planes

The layer SHALL combine, rather than substitute, these planes:

1. **Exact text plane:** paths, filenames, literals, identifiers, error strings and regular expressions.
2. **Symbol plane:** definitions, declarations, references, signatures, types, visibility and documentation.
3. **Structural plane:** syntax trees, language constructs, patterns and safe transformation targets.
4. **Relation plane:** imports, calls, inheritance, implementations, data flow, ownership and dependency edges.
5. **Semantic plane:** concept-level retrieval over symbol-aware chunks.
6. **Historical plane:** commits, blame, renames, changed symbols, prior defects and decision links.
7. **Runtime evidence plane:** tests, stack traces, coverage, profiles, build outputs and observed execution paths.
8. **Artifact plane:** schemas, API contracts, configuration, migrations, generated clients and documentation.

No single plane SHALL be treated as sufficient for code understanding.

### 19.3 Repository discovery and identity

Every indexed repository SHALL have stable identity including:

```text
repository_id
canonical_path
remote_identity
branch_or_worktree
commit_id
index_generation
language_set
configuration_hash
ignore_policy_hash
created_at
updated_at
```

A result SHALL identify the exact repository, branch, commit, path and source range it represents.

### 19.4 Incremental indexing

The layer SHALL:

- Detect file additions, changes, renames and deletions.
- Reparse only affected files and dependent symbols where safe.
- Use content hashes to avoid redundant work.
- Commit index generations atomically.
- Continue serving the last complete generation during rebuild.
- Reconcile file-watch events with periodic authoritative scans.
- Resume interrupted indexing from durable checkpoints.
- Support full rebuild from source without loss of authority.
- Prioritise active repositories and changed symbols.

No query SHALL observe a partially committed generation.

### 19.5 Language adapters

Language support SHALL be implemented through native adapters with a common contract:

```text
detect
parse
extract_symbols
extract_references
extract_relations
extract_diagnostics
slice_context
validate_change
```

An adapter SHALL expose capability levels so the system does not pretend semantic precision where only text search exists.

Required initial capability tiers:

- Tier 0: file and exact-text indexing.
- Tier 1: syntax and symbol definitions.
- Tier 2: references, types and import relations.
- Tier 3: call and dependency relations, tests and change impact.
- Tier 4: data-flow, runtime evidence and verified transformations.

### 19.6 Symbol-aware chunking

Code SHALL be segmented by semantic boundaries such as module, class, function, method, type, interface, schema, migration, test and configuration block.

Chunks SHALL include:

```text
symbol_id
fully_qualified_name
kind
signature
path
range
parent_symbol
imports
outbound_relations
inbound_relation_summary
tests
commit_id
content_hash
language
```

Arbitrary fixed-size chunking MAY be used only as fallback for unsupported formats.

### 19.7 Query planning

Each query SHALL be classified and planned. Supported intent classes SHALL include:

```text
exact_lookup
symbol_lookup
reference_search
structural_pattern
implementation_search
architecture_question
bug_localisation
change_impact
test_discovery
history_question
configuration_lookup
security_review
```

The planner SHALL select only necessary planes, fuse candidates, validate branch freshness and return an evidence-backed context pack.

### 19.8 Minimal sufficient code context

The code context assembler SHALL provide the smallest set that preserves correctness. Depending on task, it MAY include:

- Target symbol and signature.
- Direct callers or callees.
- Implemented interface or base type.
- Relevant configuration and schema.
- Closest tests and fixtures.
- Error-producing path.
- Recent change history.
- Governing project decisions.

It SHALL not dump entire directories when a symbol-level slice is sufficient.

### 19.9 Change-impact intelligence

Before a proposed change, the layer SHOULD calculate:

```text
directly_modified_symbols
direct_dependants
transitive_dependants
public_contract_changes
schema_or_migration_effects
configuration_effects
test_coverage
documentation_effects
risk_score
```

After a change, it SHALL reconcile the predicted impact with build, test and runtime evidence. Prediction errors SHALL become learning signals.

### 19.10 Index integrity and security

The layer SHALL:

- Apply repository and namespace permissions before search.
- Exclude secrets and denied paths from semantic indexes.
- Mark generated and vendored content.
- Detect stale generations.
- Prevent cross-project leakage.
- Preserve exact source attribution.
- Validate parser output against source hashes.
- Store raw source only in authorised source storage, not as an ungoverned duplicate corpus.

### 19.11 Self-improvement

The layer MAY learn ranking weights, query plans, context budgets and index priorities from:

- Accepted and rejected results.
- Agent expansions.
- Successful edits.
- Test outcomes.
- Bug-localisation accuracy.
- Human corrections.
- Latency and resource use.

It SHALL NOT autonomously alter source code or deploy parser changes. New parser or ranking versions SHALL pass a frozen benchmark corpus and shadow traffic before promotion.

### 19.12 Code intelligence metrics

Required metrics:

```text
exact_lookup_success
symbol_recall
reference_precision
bug_localisation_rank
change_impact_recall
context_tokens_per_successful_task
stale_result_rate
index_freshness
incremental_index_latency
full_rebuild_time
query_latency
cross_project_leakage
agent_expansion_rate
successful_edit_rate
```

### 19.13 Superiority gate

The Code Intelligence Layer SHALL not be labelled superior until it demonstrates:

- At least 98% success on exact path, identifier and literal lookup.
- At least 95% symbol-definition recall on supported Tier 2+ languages.
- At least 90% reference precision and recall on supported Tier 2+ languages.
- At least 25% better bug-localisation ranking than the strongest tested competing approach.
- At least 40% fewer code-context tokens per successful task than the strongest tested competing approach.
- No repository, branch or permission leakage in the security suite.
- Atomic generation consistency and complete rebuildability.
- Higher successful-edit and test-pass rates on a representative coding-agent benchmark.

Performance SHALL be reported by language, repository scale and capability tier rather than averaged into one flattering number.

---

## 20. Native Adaptive Execution Proxy

### 20.1 Governing purpose

SCOS SHALL include a native Adaptive Execution Proxy positioned between agents and eligible command or tool execution. It SHALL reduce token-heavy noise, preserve operational truth, improve command efficiency and learn which transformations produce better outcomes.

The proxy SHALL remain optional per operation and SHALL support transparent bypass.

### 20.2 Core operating flow

```text
agent request
    ↓
identity, authority and risk classification
    ↓
command or tool intent parsing
    ↓
approved rewrite and execution plan
    ↓
execution through governed tool boundary
    ↓
raw result committed
    ↓
semantic result parsing
    ↓
noise reduction, grouping and prioritisation
    ↓
compact result with raw restoration handle
    ↓
outcome and learning telemetry
```

### 20.3 Command rewriting

The proxy MAY rewrite a command only when:

- The transformation is allowlisted and versioned.
- Intended semantics are provably preserved for the recognised command form.
- Quoting, environment, working directory and exit behaviour remain correct.
- Risk classification does not require exact pass-through.
- The original command is recorded.
- A dry-run or explanation mode is available.

Examples of permitted transformation categories include:

- Add bounded output flags.
- Request machine-readable output.
- Replace recursive broad output with targeted selection.
- Group repeated diagnostics.
- Select changed or failing items first.
- Split a large read-only command into safe paged operations.
- Reuse a previously committed unchanged result by content hash.

The proxy SHALL NOT rewrite destructive, financial, legal, permission-changing or unknown commands without explicit policy and confirmation.

### 20.4 Result preservation

Before filtering, the proxy SHALL durably record or content-address the raw result when policy permits.

Every compact result SHALL preserve:

```text
operation_id
original_request
executed_request
working_directory
environment_reference
start_time
end_time
exit_status
termination_signal
stdout_reference
stderr_reference
raw_content_hash
proxy_policy_version
transformation_summary
restoration_handle
```

Exit codes, signals, errors, warnings, failed tests, security findings and stack traces SHALL never be discarded merely because they are verbose.

### 20.5 Output reduction pipeline

The proxy SHALL apply ordered transforms:

1. Parse known structured formats.
2. Separate success, warning and failure channels.
3. Remove exact repeated lines while preserving counts.
4. Collapse repeated stack frames or diagnostics with occurrence metadata.
5. Group by file, test, service, error signature or operation.
6. Prioritise failures, changed items and requested fields.
7. Replace unchanged large blocks with hashes and references.
8. Produce concise summaries plus exact restoration handles.
9. Validate that exit status and critical diagnostics remain represented.
10. Fall back to raw output when parser confidence is insufficient.

### 20.6 Native command and tool adapters

Adapters SHALL be versioned and repository-owned. Each adapter SHALL declare:

```text
supported_command_patterns
read_or_write_classification
rewrite_rules
output_parser
critical_fields
failure_signatures
restoration_strategy
validation_tests
```

Unknown commands SHALL default to transparent pass-through with raw capture rather than speculative rewriting.

### 20.7 Semantic result contracts

Where possible, compact results SHOULD use a common contract:

```text
status
summary
changed_items
failed_items
warnings
key_values
artifacts
next_recommended_action
raw_reference
```

The contract SHALL preserve the distinction between observed facts and proxy-generated recommendations.

### 20.8 Learning and evolution

The proxy MAY learn:

- Which approved rewrite performs best for a command pattern.
- Which output fields are repeatedly expanded by agents.
- Which summaries predict task success.
- Which parsers fail or omit required diagnostics.
- Which command sequence reduces retries.
- Which cache entries remain valid.

Learning SHALL produce versioned policy candidates. A candidate SHALL include evidence, expected benefit, affected commands, safety classification, regression results and rollback plan.

The promotion sequence SHALL be:

```text
observation
  ↓
policy candidate
  ↓
offline replay
  ↓
adversarial and failure testing
  ↓
shadow execution
  ↓
limited canary
  ↓
measured comparison
  ↓
approved promotion or rejection
```

The proxy SHALL never learn by directly editing its live rules in place.

### 20.9 Safety modes

Required modes:

| Mode | Behaviour |
|---|---|
| `bypass` | Execute and return raw result |
| `observe` | Execute unchanged, generate compact view for comparison |
| `safe` | Apply lossless and proven low-risk transforms |
| `adaptive` | Select among approved transformations using measured policy |
| `forensic` | Preserve and return exact raw detail with no reduction |

High-impact and incident-response operations SHALL default to `forensic` or `bypass`.

### 20.10 Proxy metrics

Required metrics:

```text
raw_output_tokens
compact_output_tokens
token_reduction
critical_diagnostic_retention
exit_status_retention
restoration_success
rewrite_success
rewrite_bypass_rate
parser_confidence
agent_expansion_rate
command_retry_rate
time_to_resolution
task_success
policy_regression_rate
```

### 20.11 Superiority gate

The Adaptive Execution Proxy SHALL not be labelled superior until it demonstrates:

- At least 70% median token reduction on supported verbose command classes.
- 100% exit-status, signal and critical-failure preservation in the conformance suite.
- 100% exact raw-result restoration for retained outputs.
- At least 30% fewer agent-requested follow-up reads than raw-output baselines.
- At least 20% lower median time-to-resolution on supported workflows.
- No increase in unsafe execution, hidden failures or duplicate side effects.
- Statistically significant task-success improvement against the strongest tested competing approach.

### 20.12 Relationship to tool governance

The proxy SHALL not bypass tool authorisation, confirmation, idempotency, audit or reconciliation. It optimises execution and representation; it does not grant authority.

---

## 21. Native Autonomous Cognitive Control Plane

### 21.1 Governing purpose

SCOS SHALL operate through a native, always-on Autonomous Cognitive Control Plane. Its purpose is to make memory, context, code intelligence, capability selection, tool mediation, persistence and learning automatic platform behaviour rather than optional agent behaviour.

The selected agent SHALL NOT need to know that memory exists, decide to call it, remember a special instruction or include a memory tool in its prompt. The control plane SHALL prepare the agent's working context before dispatch and SHALL process the result after completion.

Autonomy SHALL mean automatic coordination within granted authority. It SHALL NOT mean unrestricted side effects or removal of required human confirmation.

### 21.2 Mandatory interception boundary

All supported interaction paths SHALL enter through one of these control-plane adapters:

```text
HTTP ingress
SDK ingress
MCP host adapter
A2A adapter
CLI adapter
IDE adapter
scheduled workflow adapter
event or filesystem trigger adapter
recovery replay adapter
```

Each adapter SHALL produce the same canonical ingress event and SHALL submit it to the control plane before any model invocation.

Direct client-to-model, client-to-tool or agent-to-database paths SHALL be unsupported in production. Tool and model credentials SHALL remain behind the platform boundary.

### 21.3 Autonomous lifecycle

Every turn SHALL follow this lifecycle unless a documented policy explicitly removes an inapplicable stage:

```text
interaction_received
    ↓
authoritative_journal_commit
    ↓
identity_scope_and_policy_resolution
    ↓
intent_risk_and_freshness_classification
    ↓
capability_plan_generation
    ↓
automatic_memory_retrieval
    ↓
code_and_artifact_retrieval_when_relevant
    ↓
context_optimisation_and_invariant_validation
    ↓
agent_or_model_selection
    ↓
agent_dispatch
    ↓
autonomous_tool_loop_mediation
    ↓
output_contract_validation_and_optimisation
    ↓
reply_checkpoint_and_outbox_commit
    ↓
client_completion_signal
    ↓
post_turn_consolidation_indexing_and_learning
```

The lifecycle SHALL be durable. Each stage SHALL have a status, input reference, output reference, policy version, start time, completion time and recovery instruction.

### 21.4 Pre-turn orchestration

Before an agent receives a request, the control plane SHALL automatically:

1. Persist the original interaction.
2. Resolve tenant, user, agent, session, project, workspace, repository and data-classification scope.
3. Load the latest valid checkpoint and active task state.
4. Detect whether the request requires memory, code, artifact, external-data or tool capabilities.
5. Retrieve relevant governing knowledge, confirmed facts, prior episodes, procedures, conflicts and active constraints.
6. Select relevant code symbols, tests, schemas, configuration and history when code or project work is involved.
7. Determine freshness requirements and whether current authoritative data must be queried.
8. Select an agent or model compatible with the task, permissions, context size, latency and cost policy.
9. Compile and validate a minimal sufficient context capsule.
10. Generate an execution plan and permitted capability set.
11. Dispatch the agent only after mandatory context and policy checks succeed.

Pre-turn retrieval SHALL be automatic even when the agent would otherwise answer from its own model context.

### 21.5 Intent and task classification

Classification SHALL be multi-label. A request MAY require several systems simultaneously.

Required intent labels SHALL include:

```text
conversation
memory_recall
memory_correction
fact_lookup
fresh_external_information
code_lookup
code_change
repository_analysis
artifact_read
artifact_write
command_execution
tool_operation
workflow_continuation
planning
monitoring
recovery
high_impact_action
```

Classification SHALL use deterministic indicators, structured client metadata, active task state and an approved learned classifier. Explicit paths, repository identities, operation types and safety flags SHALL override weaker semantic guesses.

The classifier SHALL return confidence, evidence and alternative labels. It SHALL not hide uncertainty behind one compulsory category.

### 21.6 Capability registry

Every internal subsystem, agent, model, tool and connector SHALL register a versioned capability record containing:

```text
capability_id
capability_type
version
supported_intents
input_schema
output_schema
required_permissions
maximum_data_classification
read_or_write_class
side_effect_class
confirmation_policy
idempotency_support
reconciliation_support
data_locality
freshness_characteristics
cost_model
latency_profile
context_limits
health_state
historical_success_rate
quality_score
failure_modes
fallback_capabilities
```

Capabilities that are unhealthy, unauthorised, stale, incompatible or outside cost and data-egress policy SHALL be excluded before ranking.

The registry SHALL update automatically from health checks, deployments, policy changes and verified outcome telemetry.

### 21.7 Capability planning and selection

The control plane SHALL construct a capability graph rather than choose one isolated tool. A plan MAY combine memory retrieval, code intelligence, context optimisation, an agent, several read-only tools and post-turn consolidation.

Selection SHALL occur in this order:

1. Enforce hard permission, sensitivity, side-effect and user-policy constraints.
2. Identify capabilities whose declared contracts satisfy the intent.
3. Prefer authoritative and current data sources over model recollection.
4. Prefer local and lower-egress processing when quality is equivalent.
5. Prefer the smallest sufficient capability set.
6. Rank eligible plans by expected task success, correctness, evidence quality, latency, token use, monetary cost, recoverability and historical performance.
7. Reserve safe fallbacks.
8. Record the selected and rejected alternatives with reason codes.

A lower-cost plan SHALL not be selected when it materially reduces expected correctness. A more expensive plan SHALL not be selected merely because it appears sophisticated. Apparently even autonomous systems require protection from architecture theatre.

### 21.8 Default automatic routing matrix

The baseline routing policy SHALL include:

| Trigger | Mandatory automatic systems |
|---|---|
| Every incoming interaction | journal, scope resolution, active checkpoint, memory retrieval, context optimisation |
| Personal preference or prior-decision relevance | scoped semantic and episodic memory |
| Project or repository reference | project memory and code intelligence |
| Source path, symbol, stack trace or code-change request | exact, symbolic, structural and historical code retrieval |
| Proposed command or tool operation | tool governance and adaptive execution proxy |
| Current or freshness-sensitive fact | authorised current-data capability |
| Uploaded or generated artifact | artifact registry, classification and relevant index |
| Completed agent reply | durable commit, checkpoint and consolidation job |
| Tool completion | raw-result persistence, event creation, task-state update and candidate extraction |
| Repository file change | incremental code-index update and dependent-memory invalidation |
| OKF or approved policy change | validation, reindexing, cache invalidation and route-policy refresh |
| Crash or interrupted stage | checkpoint rehydration, operation reconciliation and lifecycle replay |

The matrix SHALL be extensible through versioned policy. It SHALL not require prompt edits across every agent.

### 21.9 Autonomous memory behaviour

The control plane SHALL automatically perform four distinct memory operations:

1. **Journal:** synchronously preserve every accepted interaction and operational event.
2. **Retrieve:** automatically supply relevant authorised memory before agent dispatch.
3. **Checkpoint:** commit task and conversation state after every completed response and meaningful operation.
4. **Consolidate:** asynchronously analyse completed work for episodic, semantic, procedural, revocation or OKF candidates.

Agents MAY explicitly request additional memory operations, but correct baseline operation SHALL not depend on those requests.

Automatic retrieval SHALL include negative and conflicting memories when relevant. Automatic consolidation SHALL remain governed by evidence, authority, sensitivity, duplication and approval rules.

### 21.10 Autonomous tool loop

When an agent proposes or requires a tool operation, the control plane SHALL:

1. Resolve the intended outcome rather than trusting a tool name alone.
2. Validate the proposed capability against the registry and policy.
3. Substitute a safer or more effective eligible capability when justified.
4. Add mandatory preparatory reads, dry runs or validations.
5. Obtain human confirmation when policy requires it.
6. Allocate an idempotency and operation identifier.
7. Route execution through the Adaptive Execution Proxy.
8. Persist raw and compact results.
9. Update the task state and recompile context.
10. Decide whether the agent should continue, retry, use a fallback, reconcile or stop.

The agent MAY suggest a tool. The control plane SHALL remain responsible for authorising, selecting and sequencing it.

### 21.11 Event-driven autonomous maintenance

The system SHALL subscribe to governed events and react without waiting for a conversational agent:

```text
message_committed
reply_committed
tool_completed
memory_promoted
memory_revoked
okf_changed
repository_changed
artifact_changed
capability_health_changed
policy_changed
index_stale
checkpoint_interrupted
backup_restored
```

Event handlers MAY trigger consolidation, re-embedding, incremental indexing, cache invalidation, conflict detection, route-policy refresh, health re-evaluation or recovery.

Handlers SHALL be idempotent and use the transactional outbox. Background automation SHALL not rely on a model remaining online.

### 21.12 Ambiguity and confidence policy

The control plane SHALL avoid unnecessary user questions while refusing to guess across material boundaries.

It SHALL automatically proceed when:

- The route is read-only and reversible.
- Scope and authority are clear.
- An approved conservative fallback exists.
- Missing detail does not materially alter the result.

It SHALL request human input when:

- Several interpretations would cause materially different side effects.
- Permission, identity, target or ownership is unresolved.
- A high-impact action lacks required confirmation.
- Required information cannot be recovered from authorised context.

Low confidence SHALL cause broader safe retrieval, conservative execution or a no-side-effect plan before it causes speculative action.

### 21.13 Agent contract

Agents SHALL receive a prepared execution envelope containing:

```text
objective
current_user_instruction
governing_constraints
active_task_state
selected_context_capsule
conflicts_and_uncertainty
permitted_capabilities
prohibited_actions
output_contract
operation_and_checkpoint_ids
```

Agents SHALL not be responsible for discovering the memory API, selecting namespaces, deciding whether the code index is relevant or remembering to save the reply.

An agent SHALL be replaceable without losing continuity because durable task state and orchestration remain outside the agent process.

### 21.14 Routing decision record

Every route plan SHALL create an immutable decision record containing:

```text
decision_id
interaction_id
session_id
task_id
actor_id
agent_id
resolved_scope
intent_labels
risk_class
freshness_requirement
candidate_capabilities
selected_plan
rejected_alternatives
reason_codes
confidence
policy_version
registry_generation
context_capsule_id
estimated_tokens
estimated_cost
actual_outcome
created_at
```

Operators SHALL be able to explain why a capability was used, why another was not used and what evidence changed the decision.

### 21.15 Autonomous learning and evolution

The control plane MAY improve intent classification, plan ranking, context budgets, fallback ordering and capability-health prediction from verified outcomes.

Learning signals MAY include:

```text
task success
human correction
agent retry
fallback use
retrieval expansion
test and build results
citation accuracy
tool failure
latency
token use
cost
recovery outcome
security denial
```

The live system SHALL select only approved policy versions. New learned policies SHALL pass offline replay, adversarial tests, shadow decisions, canary deployment and rollback validation.

Outcome labels generated solely by the same model being evaluated SHALL not be treated as sufficient evidence of improvement.

### 21.16 Resilience and degraded operation

The control plane SHALL define safe degraded routes:

- If semantic retrieval fails, use exact, lexical and structured memory retrieval.
- If code semantic indexes fail, use exact source and symbol metadata where available.
- If context optimisation fails, dispatch authoritative uncompressed context within model limits.
- If the preferred agent fails, use an approved compatible fallback.
- If the proxy parser fails, preserve and return raw tool output.
- If consolidation is unavailable, retain the committed journal and pending outbox job.
- If routing confidence cannot reach a safe threshold, use a conservative read-only plan or request required input.

No degraded mode SHALL weaken permission or confirmation policy.

### 21.17 Autonomy metrics

Required metrics:

```text
ingress_interception_coverage
agent_bypass_attempts
pre_turn_memory_retrieval_coverage
route_label_precision
route_label_recall
capability_selection_success
unnecessary_capability_rate
missed_required_capability_rate
human_clarification_rate
high_impact_confirmation_recall
route_planning_latency
route_replay_success
tool_substitution_success
fallback_rate
post_turn_checkpoint_coverage
consolidation_trigger_coverage
index_trigger_latency
autonomous_recovery_success
cost_per_successful_route
```

### 21.18 Autonomy acceptance gate

The autonomous platform SHALL not be considered complete until it demonstrates:

- 100% interception of supported production ingress paths.
- 100% journalling of accepted interactions before agent dispatch.
- 100% durable reply and checkpoint commit before completion signalling.
- 100% routing of tool operations through authority and proxy controls.
- Zero successful direct agent access to authoritative databases or unrestricted credentials.
- At least 98% multi-label routing precision and recall on the frozen benchmark suite.
- At least 99% correct selection of mandatory systems for known task classes.
- Zero missed confirmation requirements in high-impact conformance tests.
- Less than 2% unnecessary capability invocation on the representative suite.
- Successful continuation when tested agents contain no memory-specific instructions or tools.
- Successful recovery of interrupted orchestration from durable state.
- Reproducible route explanations and replay for every benchmark decision.

---

## 22. Native Plugin, Skill and Hook Extension Platform

### 22.1 Governing purpose

SCOS SHALL provide one native extension system through which functionality can be packaged once and installed into compatible agents without rewriting the memory core.

Supported extension classes SHALL be:

```text
plugin        executable capability or integration package
skill         reusable declarative operating procedure and knowledge bundle
hook          lifecycle-triggered handler
agent_adapter compatibility bridge for a runtime or protocol
provider      first-class replacement or augmentation point
ui_extension  permission-scoped dashboard panel or visualisation
```

Extensions SHALL use the same identity, permission, event, audit, version and rollback model as core services.

### 22.2 Extension package format

Every extension SHALL contain a signed manifest declaring:

```text
schema_version
package_id
name
version
publisher
license
extension_types
entrypoints
supported_platforms
supported_agent_boundaries
required_core_version
capabilities_provided
capabilities_consumed
hooks_subscribed
hooks_emitted
skills_exported
tools_exported
permissions
data_classes
network_destinations
filesystem_scopes
secret_references
resource_limits
dependencies
configuration_schema
health_checks
migration_plan
rollback_plan
content_hash
signature
```

Installation SHALL fail closed when the manifest is invalid, incompatible, unsigned under enforced policy, over-privileged or missing a rollback path.

### 22.3 Installation experience

The platform SHALL support:

- One-command or one-click installation from local path, Git reference, verified registry or signed bundle.
- Dry-run installation showing permissions, files, hooks, services, data access and expected changes.
- Dependency locking and reproducible installation.
- Per-agent, per-profile, per-workspace, per-project or organisation-wide activation.
- Atomic enable, disable, update and rollback.
- Side-by-side versions for canary testing.
- Exportable installation receipts.
- Complete uninstall with declared data-retention choices.
- Offline installation for self-hosted environments.

No installer SHALL silently modify an agent profile, system prompt, credentials or startup configuration.

### 22.4 Plugin isolation

Plugins SHALL execute in the least-privileged supported isolation mode:

```text
in_process_trusted
separate_process
container
wasm_sandbox
remote_endpoint
```

The selected mode SHALL be visible in the dashboard. Resource use, network calls, file access, secrets, hook latency and failures SHALL be measurable.

### 22.5 Skills

A skill SHALL be a portable, versioned bundle containing purpose, activation conditions, inputs, outputs, acceptance criteria, procedures, decision rules, required tools, permissions, relevant OKF concepts, examples, counterexamples, tests, compatibility metadata and token requirements.

Skills SHALL be discoverable by the control plane and selected automatically when their declared capabilities and activation conditions fit the task. Agents MAY request a skill, but normal operation SHALL not depend on the model remembering its name.

### 22.6 Hook system

Required lifecycle hooks SHALL include:

```text
on_install
on_enable
on_disable
on_uninstall
on_agent_start
on_agent_stop
on_session_start
on_session_switch
on_session_end
on_turn_received
on_turn_start
on_before_retrieval
on_after_retrieval
on_before_context_compile
on_after_context_compile
on_before_model
on_after_model
on_before_tool
on_after_tool
on_tool_error
on_memory_candidate
on_memory_promoted
on_memory_corrected
on_memory_revoked
on_pre_compress
on_post_compress
on_delegation_start
on_delegation_end
on_checkpoint
on_recovery
on_policy_change
on_index_change
on_user_model_change
```

Each hook SHALL declare whether it is synchronous, asynchronous, blocking, mutating, retryable and idempotent. Hook ordering SHALL be deterministic. Timeouts, failures and skipped hooks SHALL be visible.

### 22.7 Hook safety

A hook SHALL NOT increase its permissions at runtime, rewrite another extension's state without authority, block a turn indefinitely, hide authoritative events, perform undeclared external side effects or inject governing instructions from an untrusted source.

Critical hooks SHALL have fallback and circuit-breaker policies.

### 22.8 Extension discovery and capability negotiation

The control plane SHALL build a live capability graph covering core systems and installed extensions. Selection SHALL consider capability fit, permissions, trust, health, version, cost, latency, locality, data classification and historical outcomes.

A package name SHALL never be treated as a capability. Routing SHALL use declared contracts and measured behaviour.

### 22.9 Compatibility testing

Every installable extension SHALL provide conformance tests covering manifest validation, lifecycle hooks, permissions, failure isolation, upgrade, rollback, uninstall, event audit and supported agent boundaries.

---

## 23. Universal Agent Compatibility Architecture

### 23.1 Compatibility objective

SCOS SHALL provide the same autonomous memory behaviour across heterogeneous agents without requiring each agent to understand SCOS internals.

The minimum lifecycle is:

```text
capture ingress
retrieve and compile context
observe model and tool activity
persist completed turn
checkpoint state
consolidate and learn
```

### 23.2 Supported integration boundaries

| Boundary | Intended agent class |
|---|---|
| Native SDK | Agents whose source can import SCOS libraries |
| HTTP sidecar | Language-neutral applications and services |
| MCP server | MCP-capable agents and IDEs |
| Model API proxy | Agents using OpenAI-compatible or similar model endpoints |
| CLI wrapper | Terminal agents and coding CLIs |
| Provider plugin | Agents with memory or context-provider interfaces |
| Lifecycle hooks | Agents exposing turn, session or tool hooks |
| Event bridge | Agents producing logs, events or message streams |
| Session/file watcher | Last-resort integration for accessible local transcripts |
| A2A adapter | Agent-to-agent task systems |

The system SHALL prefer the richest boundary available and SHALL disclose reduced guarantees when only a weaker boundary exists.

### 23.3 Compatibility tiers

```text
Tier A  full ingress, retrieval, tools, completion, checkpoint and recovery control
Tier B  pre-turn recall and post-turn persistence with lifecycle hooks
Tier C  tool or MCP recall plus automatic transcript capture
Tier D  passive transcript ingestion and next-session context injection
Unsupported  no lawful accessible integration boundary
```

The dashboard SHALL show the tier and any missing guarantees for each agent.

### 23.4 Agent adapter contract

Every adapter SHALL implement or explicitly mark unsupported:

```text
detect
install
configure
health
identify_user
identify_agent
identify_session
capture_ingress
inject_context
observe_model_request
observe_model_response
observe_tool_call
observe_tool_result
commit_turn
checkpoint
resume
shutdown
uninstall
```

Adapters SHALL translate native events into the common SCOS event model without discarding source-specific metadata.

### 23.5 Portable context envelope

Context supplied to any agent SHALL separate governing constraints, user model, facts, learnings, current task state, episodic history, contradictions, code, artifacts, sources and restoration references.

The envelope SHALL render into the target agent's supported form without changing authority ordering.

### 23.6 Framework neutrality

Core memory SHALL not depend on framework-specific message classes, callbacks or storage. Framework adapters SHALL remain replaceable projections around the common contracts.

### 23.7 Compatibility truthfulness

The phrase "works with every AI agent" SHALL mean every agent with at least one supported accessible boundary. The project SHALL publish tested compatibility matrices, versions, limitations and conformance evidence. Untested compatibility SHALL be labelled experimental.

---

## 24. Hermes Agent Memory Provider Compatibility

### 24.1 First-class provider requirement

SCOS Nexus Memory SHALL ship a Hermes Agent memory provider package installable under the Hermes memory-provider plugin system. It SHALL operate as a single external provider and SHALL respect Hermes profile isolation.

The provider SHALL expose the name:

```text
scos-nexus
```

### 24.2 Required Hermes lifecycle mapping

The provider SHALL implement the Hermes `MemoryProvider` lifecycle:

```text
name
is_available
initialize
system_prompt_block
prefetch
queue_prefetch
sync_turn
get_tool_schemas
handle_tool_call
shutdown
on_turn_start
on_session_end
on_session_switch
on_pre_compress
on_memory_write
on_delegation
backup_paths
get_config_schema
save_config
```

### 24.3 Autonomous behaviour inside Hermes

Hermes integration SHALL NOT depend on the Hermes model choosing a memory tool.

- `prefetch()` SHALL return authorised cached recall before model calls.
- `queue_prefetch()` SHALL pre-warm likely next-turn context.
- `sync_turn()` SHALL enqueue complete turn ingestion without blocking the user interface.
- `on_pre_compress()` SHALL preserve durable insights before transcript reduction.
- `on_memory_write()` SHALL mirror authorised built-in memory changes with provenance.
- `on_delegation()` SHALL capture parent-observed subagent work.
- `on_session_switch()` SHALL preserve lineage across resume, branch, reset and compression changes.
- `shutdown()` SHALL flush bounded queues and preserve replayable pending work.

### 24.4 Hermes configuration

The setup contract SHALL require only the smallest necessary configuration:

```text
base_url
profile_scope
tenant_id
principal_id
workspace
transport_security
optional_auth_reference
```

Secrets SHALL remain in Hermes-supported secret configuration, not provider JSON or prompts.

### 24.5 Hermes tools

Explicit tools MAY be exposed for inspection and correction:

```text
scos_recall
scos_explain
scos_user_model
scos_memory_graph
scos_correct
scos_forget
scos_status
```

These tools are supplemental. Automatic recall, persistence and consolidation SHALL continue when no tool is called.

### 24.6 Hermes profile and context isolation

Storage and configuration SHALL use the active Hermes profile path supplied at initialization. Primary, subagent, cron and flush contexts SHALL be distinguished so system prompts, automated jobs and delegated work do not corrupt the user representation.

### 24.7 Hermes acceptance tests

Tests SHALL cover startup, availability without network calls, setup, prefetch latency, non-blocking sync, session switch, branch lineage, reset, pre-compression capture, delegation, built-in write mirroring, shutdown, backup paths, unavailable backend, duplicate turn sync and provider replacement.

---

## 25. Advanced Memory Intelligence and Transparency Dashboard

### 25.1 Capability objective

SCOS SHALL natively combine the strongest useful capabilities associated with modern agent-memory systems without requiring those systems as dependencies.

Required functional families:

```text
retain       ingest observations and preserve evidence
recall       retrieve relevant information with scope and time awareness
reflect      synthesise evidence-backed conclusions and revise beliefs
represent    maintain evolving models of users, agents and entities
relate       build temporal, semantic and causal links
organise     maintain summaries, diaries, hierarchies and source references
adapt        improve retrieval and memory policy from measured outcomes
explain      show what is known, inferred, used, ignored and changed
```

### 25.2 Logical memory networks

The engine SHALL maintain separately queryable networks for world facts, timestamped experiences, entity representations, beliefs and learnings, procedures and negative knowledge. These networks MAY share physical storage, but their authority and retrieval semantics SHALL remain distinct.

### 25.3 User representation

For each authorised user, the system SHALL represent explicit facts, preferences, dislikes, goals, priorities, constraints, boundaries, communication preferences, projects, roles, relationships, repeated patterns, decisions, rejected alternatives, skills, open commitments, inferred tendencies, contradictions, temporal validity and source evidence.

The representation SHALL distinguish what the user explicitly said, what a tool observed, what another source claimed and what the system inferred.

### 25.4 Peer and perspective modelling

The system SHALL support representations of any persistent entity, including users, agents and organisations. Perspective-specific knowledge SHALL preserve who believes what about whom, the evidence, scope, confidence and validity. It SHALL not silently become global truth.

### 25.5 Temporal knowledge graph

Every fact and relationship SHOULD support observed time, valid time, transaction time, source, confidence, authority and supersession.

The graph SHALL support aliases, merges, directed relations, multi-hop explanation, temporal playback, contradiction edges, evidence edges, derived-from edges, used-in-response edges, supersession, revocation and project boundaries.

### 25.6 Structured memory hierarchy

The engine SHALL provide predictable human-readable organisation:

```text
palace      complete governed knowledge space
wing        scope such as user, project or organisation
drawer      semantic collection or topic
closet      compact summary pointing to original evidence
diary       chronological synthesis
artifact    source document, message, file or result
```

These are organisational views, not separate sources of truth.

### 25.7 Reflection and belief revision

Reflection SHALL operate over cited evidence, separate premises from conclusions, record confidence, detect contradiction and staleness, create candidate beliefs, revise prior beliefs when evidence changes and preserve the derivation chain.

It SHALL avoid inferring protected or highly sensitive traits without explicit policy and legitimate purpose.

### 25.8 Background capture

The platform SHALL automatically capture authorised turns, tool calls, file changes, test outcomes, errors, decisions and task results through the control plane and hooks. Capture SHALL be asynchronous where possible and replayable after failure.

### 25.9 Real-time transparency dashboard

The system SHALL provide a futuristic, responsive dashboard backed entirely by real authorised state.

Required primary views:

```text
System Map          every service, agent, adapter, plugin and dependency
Live Trace          current turns, routes, hooks, tools and state transitions
Memory Graph        linked facts, episodes, beliefs, entities and evidence
User Model          everything authorised that the system knows about the user
Facts               explicit and observed propositions
Learnings           derived conclusions, procedures and policy improvements
Contradictions      competing claims, stale facts and unresolved conflicts
Timeline            chronological messages, actions, changes and diary entries
Agents              identities, sessions, capabilities, models and health
Plugins             installs, versions, permissions, hooks and resource use
Skills              available, selected and executed skills
Hooks               subscriptions, order, latency, failures and effects
Tool Operations     requests, approvals, execution, raw and compact results
Token Flow          source tokens, reductions, context capsules and output use
Code Intelligence   repositories, branches, index generations and symbols
Routing             selected systems, alternatives, reasons and confidence
Recovery            checkpoints, interrupted work, replay and backup health
Security            access decisions, redactions and policy enforcement
Costs               model, retrieval, storage and tool cost where available
```

### 25.10 Memory graph interaction

The graph view SHALL allow authorised users to expand relations, filter by type, source, scope, authority, confidence and time, trace conclusions to evidence, see memories used in a response, compare versions, inspect explicit versus inferred knowledge, correct or revoke entries, view downstream effects and scrub the graph through time.

### 25.11 User knowledge ledger

The User Model view SHALL present a complete ledger of authorised user knowledge grouped by facts, preferences, goals, projects, relationships, patterns, decisions, learnings, inferred beliefs and unresolved conflicts.

Every entry SHALL show content, type, explicit-or-inferred status, source, evidence, authority, confidence, validity, scope, timestamps, version, linked memories, responses that used it and correction or deletion controls.

### 25.12 Live transport

Dashboard updates SHALL use resumable Server-Sent Events or WebSocket streams. Every update SHALL include event identity, sequence, timestamp, source, tenant, scope and projection version. Reconnection SHALL resume from the last authorised sequence.

### 25.13 Visual design requirements

The dashboard SHALL be dark-first with accessible light support, responsive from mobile to multi-monitor control rooms, dense but legible, keyboard navigable, screen-reader compatible, built around live topology, graph, timeline and trace views, explicit about stale or degraded data and free of fabricated production counters or activity.

Visual spectacle SHALL not obscure causality. A glowing graph that cannot explain why a memory exists is merely a screensaver with venture funding.

### 25.14 Transparency boundary

The dashboard SHALL expose 100% of authorised operationally observable state. It SHALL not expose secrets, inaccessible tenant data, legally deleted content or private model chain-of-thought. Where hidden reasoning is unavailable, it SHALL display route reasons, evidence, policies, tool traces, confidence and concise decision summaries.

### 25.15 Competitive parity matrix

The initial named reference baselines SHALL include Hindsight, Honcho, Mem0, MemPalace and AgentMemory. They are comparative capability baselines, not required runtime dependencies. SCOS SHALL implement native equivalents and SHALL not copy vendor-specific storage authority, lock-in or opaque decision behaviour.

| Capability family | Required SCOS equivalent |
|---|---|
| Retain, recall and reflect | Evidence-backed ingestion, retrieval and synthesis |
| User and peer modelling | Global and perspective-specific entity representations |
| Graph memory | Temporal entities, relationships and derivation edges |
| Hybrid retrieval | Exact, lexical, semantic, relational and temporal fusion |
| Automatic fact extraction | Governed candidates with explicit/inferred separation |
| Hierarchical memory | Palace, wing, drawer, closet, diary and artifact views |
| Background agent capture | Turn, tool, file, test and error lifecycle hooks |
| Framework portability | SDK, HTTP, MCP, proxy, CLI, provider and event adapters |
| Adaptive memory policy | Offline evaluation, canary and reversible promotion |
| User inspection | Complete evidence-backed knowledge ledger and corrections |

Feature presence alone SHALL not establish parity. Accuracy, latency, token use, transparency, correction quality, privacy and recovery SHALL be evaluated.

---

## 26. OKF governance

Recommended structure:

```text
okf/
├── index.md
├── governance/
├── architecture/
├── projects/
├── infrastructure/
├── procedures/
├── tools/
├── decisions/
└── glossary/
```

Material changes SHALL follow proposal, validation, diff, authorised review, protected-branch merge and indexing.

Git SHALL be authoritative for approved OKF content. PostgreSQL SHALL store indexed projections including commit hash, path, metadata, content hash and import status.

Divergence SHALL be marked and reviewed. Neither side SHALL silently overwrite the other.

### 26.1 Automatic proposal generation from semantic memory

memory-intelligence MAY open an OKF proposal automatically for a fact whose
authority and confidence clear a configured threshold
(`SCOS_OKF_PROMOTION_AUTHORITY_THRESHOLD`,
`SCOS_OKF_PROMOTION_CONFIDENCE_THRESHOLD`). That threshold is only a
pre-filter; the durability judgment itself — whether the fact is governing
policy, an architecture/project/infrastructure concept, a confirmed durable
fact about a real entity, a procedure, tool documentation, a decision, or a
glossary term, as opposed to a one-off observation, unverified inference, or
ordinary conversational content — is made by the configured LLM
(`DEEPSEEK_API_KEY`), which also assigns the category, slug, title and tags.
Facts the model judges not durable enough produce no proposal.

Automatic proposal generation SHALL NOT bypass §26's proposal lifecycle: the
generated proposal still requires an independent, role-authorised review and
protected-branch merge exactly as a human-authored proposal does. This
mechanism only decides whether a proposal is worth opening and how to shape
it — never whether it merges.

---

## 27. Security model

All agents, models, retrieved documents and external tools SHALL be treated as potentially untrusted.

Every request SHALL carry human or service principal, agent identity, tenant, session, delegated authority, expiry and trace identity.

The system SHALL use application policy plus PostgreSQL Row-Level Security for sensitive multi-tenant data. Normal application traffic SHALL not use database-owner credentials.

Required data classes:

```text
public
internal
confidential
restricted
secret
```

Secret values SHALL NOT be embedded or stored as ordinary memory. Credentials SHALL live in a secret manager; memory may retain references.

Prompt injection SHALL be mitigated by separating system policy, user instruction, approved memory, tool data and untrusted retrieved content. Retrieved instructions SHALL not authorise tools.

Production SHALL use TLS, encryption at rest, environment-separated keys, key rotation, encrypted off-host backups and no secrets in Git.

Security-sensitive actions SHALL produce audit records, including permissions, promotions, deletions, restricted retrieval, exports, agent registration, policy changes, OKF approvals, restores and administrative bypasses.

---

## 28. Tool governance

Tools SHALL declare whether they are read-only, destructive, idempotent, externally side-effecting, confirmation-gated, dry-run capable and reconcilable.

State machine:

```text
proposed
prepared
authorised
executing
succeeded
failed
cancelled
unknown
reconciling
reconciled
```

Read-only tools MAY retry automatically. Idempotent writes MAY retry with the same key. Non-idempotent unknown outcomes SHALL be reconciled before retry. Financial, legal, external-message and production actions SHOULD support dry runs.

---

## 29. Multi-agent coordination

Handoffs SHALL use structured packets containing objective, constraints, authorised namespaces, memory and artifact references, completed and open work, known failures, acceptance criteria and expiry.

Shared task state SHALL be structured rather than inferred from chat history.

Every agent SHALL register identity, version, maintainer, capabilities, inputs, outputs, tool permissions, memory permissions, maximum classification, model provider, cost policy and health endpoint.

---

## 30. Crash recovery

On startup, services SHALL:

1. Verify schema and database connectivity.
2. Verify required storage.
3. Confirm OKF repository state.
4. Load latest checkpoints.
5. Identify interrupted turns.
6. Identify open or unknown tool operations.
7. Resume pending outbox jobs.
8. Reset abandoned leases.
9. Rebuild temporary caches.
10. Mark readiness only after mandatory checks succeed.

Turn recovery SHALL reconstruct events after the checkpoint, preserve committed chunks, prevent duplicate effects, rebuild context references and record the recovery action.

Authoritative writes SHALL use synchronous database commit. Durability settings SHALL not be weakened to produce prettier benchmarks.

Disk exhaustion handling SHALL reserve journal capacity, limit artifacts and indexing, alert operators and prevent uncontrolled telemetry growth.

---

## 31. Backup and disaster recovery

Production SHALL maintain:

1. Continuous PostgreSQL WAL archiving.
2. Regular base backups.
3. Encrypted off-host copies.
4. Object-store versioning or snapshots.
5. Remote Git mirrors.
6. Schema and migration exports.
7. Secret-manager recovery procedures.
8. Configuration backups.

Initial retention:

- WAL and daily recoverable backups: 35 days.
- Weekly backups: 12 weeks.
- Monthly backups: 12 months.
- OKF Git history: indefinite.
- Conversation history: namespace policy.

Backups SHALL be restored in automated integrity tests, monthly database restore tests and quarterly full recovery drills.

---

## 32. Observability

Production SHALL emit OpenTelemetry-compatible traces, metrics and structured logs.

Required metrics include journal latency, turn completion latency, retrieval latency, outbox backlog, dead letters, embedding backlog, interrupted turns, recoveries, promotion rate, duplicate rate, conflict rate, stale retrieval, permission denial, backup age and restore status.

The three native optimisation systems SHALL additionally expose token reduction, semantic and instruction retention, exact-value retention, restoration success, index freshness, symbol and reference quality, context tokens per successful coding task, critical-diagnostic retention, proxy bypass rate, policy-regression rate and cost per successful task.

The Autonomous Cognitive Control Plane SHALL expose ingress coverage, route precision and recall, mandatory-system selection, unnecessary capability rate, missed capability rate, confirmation recall, route-planning latency, bypass attempts, checkpoint coverage, trigger coverage, fallback use and autonomous recovery success.

The extension and transparency layers SHALL expose plugin health, signature state, permissions, resource consumption, hook latency and failure, skill selection, adapter compatibility tier, event-stream lag, projection freshness, graph consistency, user-model changes, belief revisions and correction propagation.

Logs SHALL correlate trace, span, session, turn, task, operation, agent and tenant IDs.

Initial targets:

| Measure | Target |
|---|---:|
| Accepted message durability after acknowledgement | 100% |
| Completed reply durability after completion signal | 100% |
| Unauthorised retrieval | 0 |
| Event schema validation | 100% |
| p95 journal append latency on LAN | <150 ms |
| p95 retrieval latency at initial scale | <750 ms |
| Scheduled restore success | 100% |
| Lost committed turns in crash tests | 0 |

---

## 33. Quality evaluation

A versioned evaluation suite SHALL include known-answer retrieval, exact identifiers, temporal questions, contradictions, permissions, project isolation, multi-turn continuity, recovery, stale memory and prompt-injection cases.

Track Recall@K, Precision@K, MRR, nDCG, citation accuracy, authority accuracy, conflict detection, duplicate rate, stale-hit rate, promotion precision/recall, token efficiency and answer regression.

A dedicated comparative benchmark programme SHALL test the Context Optimisation Layer, Code Intelligence Layer, Adaptive Execution Proxy and Autonomous Cognitive Control Plane against an unoptimised baseline, the prior production version and the strongest relevant competing implementations available for lawful evaluation. The suite SHALL include ordinary, adversarial, long-context, large-repository, failure-heavy and recovery scenarios.

Results SHALL report confidence intervals, dataset composition, exclusions, hardware, model, version, cost, latency and task-success criteria. Benchmark corpora SHALL be frozen before final comparative runs. Cherry-picked demonstrations SHALL not count as evidence.

Automatic promotion or learned-policy deployment SHALL be narrowed or disabled when unsupported facts, sensitive misclassification, high duplicate rate, weak conflict detection, instruction loss, diagnostic loss, restoration failure, benchmark regression or excessive human rejection appear.

---

## 34. Governance roles

Logical roles:

- **System owner:** direction, governing policy, architecture and risk acceptance.
- **Memory steward:** taxonomy, promotion, retention, correction and retrieval quality.
- **Security steward:** identity, permissions, threats, secrets and incidents.
- **Knowledge curator:** OKF quality, sources, concepts and deprecation.
- **Agent maintainer:** registration, interfaces, permissions and evaluation.
- **Autonomy steward:** routing policy, capability contracts, confidence thresholds, route quality and safe fallback behaviour.
- **Operator:** deployment, monitoring, backups, recovery and capacity.
- **Auditor:** independent review of privileged actions and policy compliance.

One person MAY initially hold several roles, but the responsibilities SHALL remain distinct.

---

## 35. Change governance

Architecture Decision Records SHALL cover authoritative storage, event semantics, taxonomy, permissions, retention, encryption, providers, brokers, databases, embedding migrations, shared scopes, autonomous authority, ingress interception, capability registry contracts, route policy, confidence thresholds, token-optimisation policy, code-index schema, proxy rewrite policy, learning signals and superiority benchmark changes.

Material changes SHALL define compatibility, migrations, rollback or forward-fix strategy, index rebuild and verification.

Security-critical changes require explicit owner and security approval.

---

## 36. Implementation baseline

### Core service

- Go for the Memory Gateway, turn coordination and policy enforcement.
- A native Go Autonomous Cognitive Control Plane for ingress interception, capability planning, lifecycle coordination, routing decisions and recovery.
- Native SCOS services for context optimisation, code intelligence and adaptive execution proxying.
- Python for consolidation, embeddings, reranking, evaluation and offline policy training where appropriate.
- TypeScript, Python and Go SDKs as needed.
- Core operation of all three native optimisation systems SHALL remain local and self-hostable without an external hosted dependency.

### Data and infrastructure

- PostgreSQL 17+.
- pgvector.
- Native PostgreSQL full-text search.
- Row-Level Security.
- WAL archiving and optional streaming replica.
- S3-compatible object storage.
- Git-backed OKF.
- OpenTelemetry Collector.
- Docker Compose and systemd initially.

### Deferred until justified

- Kafka.
- Dedicated search cluster.
- Dedicated graph database.
- Kubernetes.
- Multi-region active-active writes.
- Autonomous OKF merges.

---

## 37. Repository layout

```text
scos-memory-fabric/
├── GOVERNANCE.md
├── README.md
├── services/
│   ├── memory-gateway/
│   ├── autonomous-control-plane/
│   ├── extension-manager/
│   ├── compatibility-gateway/
│   ├── memory-intelligence/
│   ├── transparency-projection/
│   ├── dashboard/
│   ├── context-optimisation/
│   ├── code-intelligence/
│   └── adaptive-execution-proxy/
├── workers/
│   ├── consolidation/
│   ├── index-maintenance/
│   └── policy-evaluation/
├── packages/
│   ├── context-contracts/
│   ├── routing-contracts/
│   ├── capability-registry/
│   ├── extension-contracts/
│   ├── hook-sdk/
│   ├── skill-sdk/
│   ├── agent-adapter-sdk/
│   ├── hermes-memory-provider/
│   ├── language-adapters/
│   ├── execution-adapters/
│   └── benchmark-harness/
├── plugins/
├── skills/
├── hooks/
├── adapters/
│   ├── hermes/
│   ├── mcp/
│   ├── http/
│   ├── model-proxy/
│   └── cli-wrapper/
├── policies/
│   ├── context/
│   ├── routing/
│   ├── extensions/
│   ├── user-model/
│   ├── code-index/
│   └── execution-proxy/
├── dashboard/
├── benchmarks/
├── indexes/
├── schemas/
│   ├── events/
│   ├── memory/
│   ├── routing/
│   ├── extensions/
│   ├── dashboard/
│   └── handoffs/
├── database/
│   ├── migrations/
│   └── policies/
├── okf/
├── deploy/
├── tests/
└── docs/
```

---

## 38. Required interfaces

Required HTTP areas:

```text
/v1/sessions
/v1/turns
/v1/memory
/v1/okf
/v1/operations
/v1/audit
/v1/retrieval-runs
/v1/control/ingest
/v1/control/route
/v1/control/decisions
/v1/control/replay
/v1/control/recover
/v1/capabilities
/v1/triggers
/v1/context/compile
/v1/context/restore
/v1/context/evaluate
/v1/code/index
/v1/code/query
/v1/code/impact
/v1/proxy/execute
/v1/proxy/restore
/v1/proxy/explain

/v1/benchmarks
/v1/extensions
/v1/extensions/install
/v1/extensions/{package_id}/enable
/v1/extensions/{package_id}/disable
/v1/extensions/{package_id}/rollback
/v1/extensions/{package_id}
/v1/skills
/v1/hooks
/v1/hooks/executions
/v1/adapters
/v1/compatibility
/v1/entities
/v1/entities/{entity_id}/representation
/v1/users/{user_id}/knowledge
/v1/memory/graph
/v1/beliefs
/v1/learnings
/v1/diary
/v1/transparency/snapshot
/v1/transparency/events
/v1/transparency/stream
/health/live
/health/ready
```

MCP and other adapters SHALL connect to the Autonomous Cognitive Control Plane, which SHALL invoke memory, code, context and proxy capabilities automatically. Optional explicit tools MAY include memory recall, candidate submission, correction, history, route explanation, task checkpoint/resume, artifact registration and OKF search, but normal continuity SHALL not depend on an agent calling them.

Promotion, deletion, permission changes, OKF merges and policy changes SHALL remain privileged.

---

## 39. Failure-injection acceptance tests

The system SHALL pass tests that kill processes during message acceptance, retrieval, tools, streaming, completion, consolidation and handoff. It SHALL tolerate duplicate outbox jobs and events, altered idempotency retries, storage outages, model-provider loss, disk pressure, index destruction, embedding-model migration, point-in-time restore, cross-project access attempts, prompt injection, secret embedding and contradictory instructions.

It SHALL additionally pass tests for lost mandatory instructions during compression, altered exact values, broken restoration handles, tokenizer mismatch, stale or partial code generations, parser crashes, repository renames, cross-branch leakage, malformed syntax, proxy parser uncertainty, hidden error output, changed exit status, unsafe rewrite attempts, duplicate side effects, poisoned learning signals, policy rollback and benchmark reproducibility.

Autonomy tests SHALL include agents with no memory prompt or memory tools, direct-path bypass attempts, missing capability health, conflicting route labels, ambiguous project scope, required current-data lookup, code-and-tool compound tasks, high-impact confirmation, route-policy rollback, event-trigger duplication, interrupted route recovery and deterministic decision replay.

Extension tests SHALL include malicious manifests, invalid signatures, dependency conflicts, hook deadlocks, hook ordering, plugin crashes, resource exhaustion, secret-access attempts, upgrade migration failure, rollback, uninstall residue, incompatible agent versions and cross-profile leakage.

Dashboard and intelligence tests SHALL include event-stream interruption, projection lag, graph inconsistency, correction propagation, superseded beliefs, stale user facts, explicit-versus-inferred labelling, unauthorised graph traversal, tenant isolation, secret redaction and zero fabricated production telemetry.

Hermes tests SHALL include provider discovery, setup, profile isolation, automatic prefetch, non-blocking turn sync, session switch, branch lineage, reset, pre-compression preservation, built-in write mirroring, delegation, shutdown and backend recovery.

Expected outcome:

- No committed turn disappears.
- No unauthorised memory is exposed.
- No non-idempotent side effect is blindly duplicated.
- Derived indexes can be rebuilt.
- Conflicts remain visible.
- Recovery remains auditable.

---

## 40. Delivery phases

1. **Governance and contracts:** schemas, API, threat model, ADRs and recovery plan.
2. **Durable journal:** sessions, turns, messages, chunks, events, checkpoints, idempotency and outbox.
3. **Scoped memory:** namespaces, ACLs, versions, evidence, correction and retention.
4. **Autonomous control plane:** mandatory ingress, capability registry, multi-label routing, lifecycle state, automatic context hydration, tool-loop mediation and decision audit.
5. **Native context optimisation:** lossless compiler, capsules, restoration, budgeting, output contracts and quality gates.
6. **Native code intelligence:** repository identity, exact and symbolic indexes, structural parsing, relations, incremental generations and minimal context assembly.
7. **Native adaptive execution proxy:** raw capture, deterministic adapters, compact results, restoration, safety modes and telemetry.
8. **Hybrid retrieval:** exact, full-text, vectors, filters, fusion and evaluation.
9. **Consolidation:** extraction, deduplication, conflict handling, authority and review.
10. **OKF:** Git repository, validation, import/export and approval workflow.
11. **Governed learning:** offline replay, policy candidates, shadow evaluation, canary and rollback.
12. **Multi-agent integration:** SDKs, MCP, A2A, task state and agent registry.
13. **Production resilience:** WAL, PITR, replicas, telemetry, alerts and failure injection.
14. **Superiority programme:** frozen benchmarks, comparative evaluation and public internal scorecards.
15. **Extension platform:** signed manifests, plugin manager, skills, hooks, permissions, isolation and rollback.
16. **Universal compatibility:** SDK, HTTP, MCP, proxy, CLI, provider and event adapters with tiered conformance.
17. **Hermes provider:** first-class MemoryProvider package and lifecycle tests.
18. **Advanced memory intelligence:** entity representation, temporal graph, beliefs, reflection, diary and hierarchical views.
19. **Transparency dashboard:** live projections, graph, user ledger, traces, plugins, hooks, tools, tokens, code and recovery.
20. **Advanced capabilities:** additional ranking and federation only when justified.

---

## 41. Definition of done

The first production release is complete only when:

- Accepted messages and completed responses are durably committed.
- Interrupted streams can be reconstructed.
- Tools have durable state, idempotency and reconciliation.
- Checkpoints restore sessions.
- Memory is namespace-scoped and RLS-tested.
- Full-text and vector retrieval are combined.
- Retrieval returns provenance and conflicts.
- Corrections preserve versions.
- Promotion is governed.
- OKF is Git-backed.
- Search indexes rebuild from authority.
- Backups have been restored successfully.
- Failure injection passes.
- Agents and clients enter through the Autonomous Cognitive Control Plane rather than direct model, tool or database paths.
- Memory retrieval, context hydration, reply persistence, checkpointing and consolidation happen without agent prompting.
- Capability selection is automatic, explainable, permission-aware and benchmarked.
- Agents with no memory awareness retain full continuity across turns and crashes.
- Recovery and incident runbooks exist.
- Context optimisation preserves mandatory instructions, exact values and evidence while meeting the approved token-reduction targets.
- Every compressed context and tool result can restore its authoritative source.
- Code indexes are branch-aware, atomic, permission-scoped and rebuildable.
- Code retrieval meets approved exact, symbol, reference, impact and token-efficiency targets.
- The execution proxy preserves exit state and critical diagnostics and safely bypasses unknown operations.
- Learning changes are versioned, evaluated, canaried and reversible.
- The three native systems beat the approved comparative baselines without lowering task success, correctness or safety.
- Plugins, skills, hooks and adapters install, update, roll back and uninstall through signed governed packages.
- Supported agents receive automatic memory through at least one documented compatibility boundary.
- Compatibility tier and missing guarantees are visible for every connected agent.
- Hermes loads SCOS Nexus as a first-class memory provider and passes its complete lifecycle suite.
- Retain, recall, reflect, entity representation, temporal graph, belief revision, diary and hierarchical memory pass comparative benchmarks.
- Every authorised user fact, inference, learning and relationship is inspectable, sourced, versioned and correctable.
- The dashboard streams actual system state in real time and traces every displayed conclusion to authority or derived evidence.
- No production dashboard panel uses fabricated data.
- Dashboard transparency respects tenant, permission, deletion, secret and private-reasoning boundaries.

---

## 42. Governing prohibitions

The following are prohibited:

- Writing every reply directly into semantic memory.
- Allowing unsupported shared-memory mutation.
- Embedding secrets.
- Treating vector similarity as authority.
- Hiding contradictions.
- Deleting history during correction.
- Signalling completion before commit.
- Blindly retrying unknown side effects.
- Giving agents database-owner credentials.
- Mixing private and shared scopes.
- Making embeddings the only copy.
- Adding infrastructure because its diagram looks impressive.
- Calling untested backups complete.
- Replacing evidence with agent summaries.
- Feeding entire histories into every prompt.
- Persisting hidden reasoning.
- Allowing retrieved documents to override policy.
- Weakening durability settings for benchmarks.
- Claiming superiority without reproducible comparative evidence.
- Optimising token count at the expense of task success or required detail.
- Dropping governing instructions, exact values, evidence, errors or warnings during compression.
- Creating irreversible summaries or filtered outputs without restoration handles.
- Using arbitrary fixed-size chunks when a supported semantic code boundary exists.
- Serving partially committed or cross-branch code indexes.
- Rewriting unknown or destructive commands speculatively.
- Allowing the adaptive proxy to bypass authority, confirmation, idempotency or audit.
- Letting learned policy modify live executable rules without governed promotion.
- Making core operation of the three native optimisation systems dependent on an external hosted service.
- Requiring agents to remember to call memory for normal continuity.
- Allowing supported ingress to bypass the Autonomous Cognitive Control Plane.
- Allowing a model to grant itself permissions or select prohibited tools.
- Treating a single semantic classifier as sufficient authority for high-impact routing.
- Running post-turn persistence or checkpointing only when requested by the agent.
- Hiding autonomous route decisions or making them impossible to replay.
- Silently performing confirmation-gated side effects under the label of autonomy.
- Installing extensions without a manifest, permission review, compatibility check and rollback path.
- Treating skills as unversioned prompt fragments.
- Allowing hooks to run with undeclared authority or unlimited blocking time.
- Claiming compatibility with an agent that exposes no tested accessible boundary.
- Requiring Hermes to call an explicit memory tool for ordinary recall or persistence.
- Conflating inferred user traits with explicit facts.
- Promoting perspective-specific beliefs into global truth without evidence.
- Displaying fabricated dashboard metrics, graph nodes, live events or user knowledge.
- Calling hidden chain-of-thought operational transparency.
- Hiding which memories, plugins, skills, hooks, agents or tools influenced a response.
- Preventing an authorised user from inspecting, disputing, correcting or deleting eligible user-model entries.

---

## 43. Final architecture decision

SCOS Memory Fabric SHALL be:

> **A PostgreSQL-backed, event-journalled, transactionally checkpointed, permission-aware autonomous agent memory platform whose native cognitive control plane intercepts every interaction, automatically selects and coordinates memory, context, code, agents and tools, supports signed plugins, portable skills, deterministic hooks, universal agent adapters and a first-class Hermes memory provider, and provides retain-recall-reflect intelligence, temporal user and entity knowledge graphs, reversible token optimisation, repository-scale code intelligence, adaptive execution proxying, hybrid retrieval, controlled consolidation, Git-backed OKF governing knowledge and a real-time transparency dashboard.**

```text
Events preserve what happened.
Checkpoints preserve where work stopped.
Memories preserve what remains useful.
The control plane decides what must happen without waiting for an agent to remember.
Extensions add capability without bypassing governance.
Skills package reusable intelligence; hooks bind it to lifecycle events.
Adapters make the memory lifecycle portable across agents.
Hermes receives memory automatically through its provider lifecycle.
Retain records evidence; recall retrieves context; reflect revises understanding.
The user ledger shows every authorised fact, inference, learning and link.
The dashboard shows what every observable system is doing in real time.
Context capsules minimise tokens without losing authority.
Code intelligence finds the smallest sufficient source truth.
The execution proxy removes noise without hiding operational truth.
OKF preserves what has been approved.
Evidence preserves why it should be believed.
Policies preserve who may use it.
Benchmarks prove whether the system is actually superior.
```

This document is the governing baseline. Implementation that conflicts with it requires an approved ADR or an amended governing version.
