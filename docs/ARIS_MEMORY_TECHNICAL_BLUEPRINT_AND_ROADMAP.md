# ARIS Memory Fabric Technical Blueprint and Phased Roadmap

**Repository:** `dylmarriner/scos-memory`
**Role:** Authoritative durable memory fabric for ARIS
**Cross-repo authority:** `dylmarriner/ARIS/docs/ARIS_MASTER_TECHNICAL_BLUEPRINT.md`

## 1. Mission

SCOS Memory is the governed durable memory plane for ARIS. It persists the record of interactions, tasks, observations, evidence, promoted knowledge and memory provenance while preserving clear authority over what is merely observed, what is inferred and what has been promoted to durable knowledge.

It is not the ARIS Executive and must not become a second general cognitive control plane. Harness owns task orchestration, planning, tool selection and runtime cognition. SCOS Memory owns durable memory lifecycle, retrieval, promotion, consolidation, provenance and recovery.

The defining rule is:

> Persist every relevant event safely. Promote only evidence-backed durable knowledge.

## 2. Canonical ownership

This repository owns:

- durable interaction/task journal.
- episodic memory.
- semantic memory.
- provenance and evidence lineage.
- governed knowledge promotion.
- contradiction/supersession records.
- scope/tenant/identity-aware retrieval.
- consolidation workers.
- embedding/index generation.
- OKF-backed portable approved knowledge.
- memory recovery and idempotent writes.
- memory audit/governance.
- memory-facing APIs/adapters.

It does **not** own:

- global task scheduling.
- ARIS goal lifecycle.
- model/agent routing.
- privileged host execution.
- ARIS policy authority outside memory-specific access/promotion rules.
- native-model training.
- whole-system resource allocation.

## 3. Memory classes

### Working memory

Owner: ARIS Harness.

Lifetime: current task/session/checkpoint.

Examples:

- active hypotheses.
- current tool outputs.
- short-lived context.
- pending actions.
- intermediate candidate answers.

SCOS may receive checkpoint/journal events but does not need to turn every intermediate token into semantic memory.

### Episodic memory

Owner: SCOS Memory.

Represents what happened:

- task started/completed/failed.
- action attempted.
- tool result.
- agent/model invocation.
- verification outcome.
- user correction.
- system event linked to a task.

Episodic events are durable facts about history, not necessarily reusable truths about the world.

### Semantic memory

Owner: SCOS Memory.

Represents promoted durable knowledge:

- user preferences.
- machine facts.
- project facts.
- stable configuration knowledge.
- verified conclusions.
- relationships.

Promotion requires evidence, scope and conflict checks.

### Procedural memory

Logical owner: ARIS Harness skill system.

SCOS responsibility:

- persist skill metadata/provenance where required.
- support retrieval/version references.
- retain evidence and promotion history.

Harness owns execution and skill evaluation.

### Model memory

Owner: ARIS Intelligence weights/releases.

SCOS may supply verified curricula or approved knowledge exports but does not directly mutate serving model weights.

## 4. Authoritative stores

Canonical authorities:

- PostgreSQL: durable operational/event/memory record.
- OKF Git store: approved portable/versioned knowledge where applicable.

Derived/rebuildable systems:

- pgvector indexes.
- embedding stores.
- full-text indexes.
- caches.
- ranking projections.

A vector index is never the source of truth.

## 5. Memory lifecycle

Canonical semantic promotion flow:

```text
observation / task event
 -> durable journal
 -> evidence collection
 -> candidate extraction
 -> scope resolution
 -> conflict/contradiction check
 -> quality/policy check
 -> promotion proposal
 -> approval/automated rule where permitted
 -> durable semantic/OKF record
 -> indexes regenerated/updated
```

Never use:

```text
LLM output -> permanent fact
```

without evidence and promotion controls.

## 6. Journal contract

The journal is append-oriented and reconstructable.

Each material event should carry stable lineage such as:

```text
schema_version
memory_event_id
trace_id
session_id
goal_id
task_id
action_id where relevant
source
identity/scope
occurred_at
received_at
provenance/evidence refs
payload type
```

Idempotency keys prevent duplicate commits during retry/reconnect.

## 7. Harness memory API

Harness should interact through versioned operations such as:

### Journal

- append task/runtime event.
- append verification result.
- append user correction.
- append reflection outcome.

### Retrieve

- scoped semantic query.
- episodic query.
- evidence query.
- entity/history query.
- skill/procedure reference query.

### Promote

- submit memory candidate.
- attach evidence.
- inspect conflicts.
- approve/reject/supersede according to policy.

### Health

- live/ready/degraded state.
- authoritative store health.
- index lag.
- worker backlog.

Harness must not depend on internal SCOS database schemas.

## 8. Context retrieval

Retrieval should return structured evidence, not only text chunks.

A result should expose where possible:

- memory ID.
- memory class.
- content/structured data.
- scope.
- provenance.
- confidence/quality.
- observed/effective dates.
- supersession/contradiction status.
- retrieval/ranking reason.

Harness then decides which evidence belongs in model context.

Memory does not own prompt construction.

## 9. Identity and scope

Every durable write/read is scoped.

Potential dimensions:

- logical ARIS identity.
- user/person.
- device/node.
- project/repository.
- organization/workspace.
- task/session.
- privacy class.

The storage layer should enforce isolation where deployment configuration supports it, not rely only on prompt conventions.

## 10. Contradiction and supersession

Durable knowledge changes over time.

Do not silently overwrite facts without lineage.

Support relations such as:

```text
supersedes
contradicts
confirms
derived_from
observed_from
applies_to
```

Example:

```text
old belief: node X has 16 GB RAM
new verified observation: node X has 32 GB RAM
```

The new record supersedes the old current-state belief while preserving historical provenance.

## 11. Offline/disconnected operation

ARIS must remain useful while Memory Fabric is unavailable.

The ARIS repository owns the local memory gateway/cache/outbox integration. SCOS Memory must support safe reconciliation.

Required server-side behaviour:

- idempotent write replay.
- monotonic/event ordering where needed.
- conflict detection.
- duplicate suppression.
- lease/recovery semantics for workers.
- explicit acceptance/rejection responses.

A local cache is not a second authority.

## 12. Consolidation

Consolidation workers can:

- summarize episodic history.
- identify repeated facts.
- cluster related evidence.
- generate semantic candidates.
- detect contradictions.
- propose procedural lessons.

They cannot unilaterally increase authority merely because an LLM generated a polished summary.

All promoted output retains provenance to source events/evidence.

## 13. OKF role

The OKF Git store is appropriate for approved, portable, reviewable knowledge that benefits from:

- version history.
- diffs.
- rollback.
- human inspection.
- export/backup.

Do not force every transient episodic event into Git. PostgreSQL remains the operational authority.

## 14. Embeddings and search

Use hybrid retrieval where useful:

- exact/structured lookup.
- relational queries.
- full-text search.
- vector similarity.
- recency.
- scope filtering.
- provenance/quality filters.

Ranking should never bypass access scope or contradiction state.

Embedding-model upgrades must be rebuildable without changing memory truth.

## 15. Memory promotion policy

Candidate scoring may consider:

- verification strength.
- number/quality of evidence sources.
- source authority.
- stability over time.
- contradiction state.
- user confirmation where appropriate.
- scope.
- reuse value.

Promotion thresholds can vary by memory type. A harmless preference and a security-sensitive machine fact need not share identical policy.

## Owner-Governed Evolution evidence contract (2026-10-25)

[ARIS Owner-Governed Evolution Engine](https://github.com/dylmarriner/ARIS/blob/main/docs/architecture/owner-governed-evolution.md) makes real self-modification an explicit product capability. SCOS Memory is the **durable evidence/provenance plane**, not the authority that approves an experiment or deploys a change.

Memory may retain and retrieve revision-linked proposals, experiments, baseline-versus-candidate scores, resource/API costs, verified outcomes, failed attempts, regressions, owner decision references, adopted skills, and rollback/lineage. Candidate improvements may target any ARIS subsystem, including Memory itself. Do not impose arbitrary idea-level proposal denials through memory retrieval or consolidation.

**Default design preference:** owner approval for **testing only**, followed by test-result review and a **separate** approval for deployment. An owner preference or a memory record is **never** itself an executable TEST or DEPLOY grant. SCOS cannot forge, mutate or issue owner proof. Authoritative authentication/grants belong to ARIS's independent decision/enforcement path; Memory stores references/hashes/audit evidence only.

Memory consolidation may learn from approved, rejected and failed proposals as evidence, but must not independently apply candidate source changes, activate skills, overwrite active model weights or install updates. Provenance must bind candidate hash, origin trace, experiment and evaluator versions, result artifacts, decision reference and supersession/rollback records. Sensitive diffs, credentials and logs remain scoped/redacted.

## 16. User correction and deletion

Memory must support correction/supersession workflows with audit history.

Where product policy requires deletion, deletion should propagate through:

- authoritative record policy.
- derived indexes.
- caches.
- exports where feasible.
- retraining pipelines where relevant and governed separately.

Do not pretend deleting a vector row alone deletes knowledge.

## 17. Security boundary

Treat memory writes from models/agents as untrusted candidate content.

Threats include:

- prompt-injected false memories.
- malicious tool output.
- poisoned documents/webpages.
- compromised agent assertions.
- cross-user scope leakage.
- forged provenance.
- replay attacks.

Required controls:

- authenticated callers.
- scoped authorization.
- schema validation.
- provenance integrity.
- idempotency.
- RLS/data-layer isolation where configured.
- audit.
- secret-free content policies where possible.

## 18. Observability

Track:

- journal ingest latency.
- write failures/retries.
- outbox replay success.
- query latency.
- retrieval hit quality.
- index lag.
- consolidation backlog.
- promotion/rejection counts.
- contradiction rate.
- stale-memory detection.
- worker recovery.

Trace IDs from Harness should survive through journal, retrieval and promotion operations.

## 19. Deployment model

Support at least:

- local single-host Compose/dev deployment.
- native systemd deployment.
- remote home-lab/server deployment.
- future scaled service deployment only when required.

Do not require Kubernetes for ordinary ARIS use.

ARIS OS may run a local gateway while the authoritative SCOS services live on another trusted node.

## 20. Supporting repository migration

### `synapse-memory`

Treat as legacy/experimental.

Audit useful features/data models and migrate only what improves the canonical Memory Fabric. Avoid dual writes to two permanent memory authorities.

### `shared-memory-mcp`

Long-term role:

```text
MCP clients
 -> shared-memory-mcp adapter
 -> SCOS Memory API
```

It should not maintain divergent durable truth.

## 21. Phased roadmap

### M0 - Authority and terminology lock

- explicitly define SCOS as governed Memory Control Plane, not global ARIS cognitive control plane.
- document Harness/Memory boundary.
- catalogue existing services/workers/adapters.

**Exit:** no documentation implies SCOS owns ARIS-wide task/tool orchestration.

### M1 - Stable memory contracts

- journal schemas.
- retrieval schemas.
- candidate/promotion schemas.
- scope/identity model.
- health/version endpoint.

**Exit:** Harness can integrate without depending on database internals.

### M2 - Durable journal hardening

- idempotency.
- transactional event commits.
- outbox/inbox patterns where needed.
- recovery leases.
- failure-injection coverage.

**Exit:** retries/crashes cannot silently duplicate or lose committed task events.

### M3 - Episodic memory

- task/session/action history queries.
- provenance/evidence links.
- time-window/entity retrieval.

**Exit:** ARIS can reconstruct what happened in a prior task with source lineage.

### M4 - Semantic promotion

- candidate extraction interface.
- conflict checks.
- approval/promotion pipeline.
- supersession/contradiction model.
- OKF integration.

**Exit:** verified knowledge can be promoted without turning raw model output into truth.

### M5 - Hybrid retrieval

- structured/full-text/vector retrieval.
- quality/recency/provenance ranking.
- index rebuild tooling.

**Exit:** Harness receives scoped evidence-rich results with rebuildable indexes.

### M6 - ARIS offline reconciliation

- support local-gateway outbox replay.
- idempotent reconnect.
- conflict responses.
- cache freshness metadata.

**Exit:** temporary Memory outage does not lose task journals or create competing truth.

### M7 - Consolidation and reflection support

- episodic consolidation.
- semantic candidate proposals.
- repeated-pattern detection.
- skill-lesson/procedure candidate support for Harness.
- evolution experiment/proposal/result provenance, including rejected and failed candidates without treating them as permission to execute.

**Exit:** long histories become useful candidates while promotion remains governed; Memory never automatically deploys learned self-modifications.

### M8 - Governance and transparency

- memory browser/transparency projection.
- promotion/provenance inspection.
- correction/supersession workflows.
- governance audit gates.
- owner-decision references and exact evaluated candidate versions are inspectable, but approvals remain independently enforced outside Memory.

**Exit:** important durable memories and evolution experiments can be inspected, traced and corrected without becoming a second Executive.

### M9 - Production resilience

- backup/restore drills.
- corruption/failure scenarios.
- scaling limits.
- incident runbooks.
- security review.
- upgrade/migration compatibility.

**Exit:** Memory Fabric is safe to rely on as the durable ARIS memory authority.

## 22. First ARIS integration acceptance test

Scenario:

```text
Harness performs verified Wi-Fi repair
 -> journal task/actions/evidence
 -> submit candidate: known recovery procedure / machine fact
 -> SCOS checks scope/provenance/conflicts
 -> promote appropriate memory
 -> restart Harness
 -> query relevant prior experience
 -> retrieve evidence with provenance
```

This proves memory survives model/runtime restart without becoming uncontrolled self-belief.

## 23. API design principles

- version contracts explicitly.
- prefer stable IDs over textual references.
- make idempotency first-class.
- return provenance with knowledge.
- expose health/degraded state.
- do not leak storage implementation details into clients.
- separate journal write from semantic promotion.
- make scope explicit on every material operation.

## 24. Definition of success

This repository succeeds when ARIS can durably remember what happened, retrieve relevant evidence, promote verified knowledge, preserve provenance and contradiction history, survive crashes/retries/outages, support correction and transparency, and do all of that without becoming a competing cognitive runtime.


## Evolution Engine evidence and context integration (proposed, 2026-10-29)

The [ARIS Evolution Engine SCOS contract](ARIS_EVOLUTION_ENGINE_SCOS_CONTRACT.md) refines SCOS's evidence/provenance and incremental context-learning responsibilities under [ARIS-EE-BP-001](https://github.com/dylmarriner/ARIS/blob/main/docs/evolution/ARIS_EVOLUTION_ENGINE_TECHNICAL_BLUEPRINT_AND_ROADMAP.md). It builds on the existing Owner-Governed Evolution evidence contract and [SCOS ADR-0009](adr/0009-governed-learning-and-promotion.md), rather than changing accepted approval policy. SCOS retains immutable raw source references, revisioned/trace-backed playbooks, negative experiments and evaluation artifacts. **Approval evidence stored in SCOS is not a TEST or DEPLOY grant.** The trusted evaluator and authorization verifier remain independently enforced. EE-P0–EE-P9 is a separate cross-repository implementation track from SCOS P0–P8, and the proposed interfaces are not claims of completion.
