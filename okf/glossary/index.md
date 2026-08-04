---
title: Glossary
description: Definitions of terms used throughout GOVERNANCE.md and the SCOS Memory Fabric codebase, kept consistent with the governing document.
tags: [glossary, terminology]
status: active
---

# Glossary

Terms are defined as used in `GOVERNANCE.md`. Where this glossary and the
governing document conflict, `GOVERNANCE.md` is authoritative and this file
is in error.

**Authority** — A memory's position in the conflict-precedence hierarchy
(GOVERNANCE.md §10), from current explicit human instruction down to
unsupported model candidate. Distinct from *confidence*.

**Capability registry** — The versioned record of every internal subsystem,
agent, model, tool and connector eligible for routing (§21.6).

**Checkpoint** — A durable, atomic snapshot of session/task state committed
after a completed reply or meaningful operation, used to resume interrupted
work (§11.5, §21.9).

**Confidence** — A statement's estimated reliability, independent of its
authority. A high-confidence single observation still ranks below a
lower-confidence approved OKF concept.

**Consolidation** — The asynchronous, governed process of extracting
candidate facts/decisions/preferences/procedures/outcomes from completed
work and promoting, merging, quarantining, rejecting, or routing them for
review (§16).

**Context capsule** — An immutable, auditable record of a lossy or selective
context view, carrying source IDs, hashes, versions, compression profile and
restoration handles (§18.5). A capsule never replaces its source.

**Control plane** (Autonomous Cognitive Control Plane) — The mandatory,
always-on ingress and orchestration layer that journals, classifies, plans,
routes, dispatches, commits and recovers every interaction without
depending on agent-initiated memory calls (§21).

**Evidence** — A concrete, addressable reference (source, tool result,
observation) supporting a memory's factual claim; required for every
promoted semantic or governing memory unless explicitly marked an
unsupported hypothesis (§13).

**Journal** — The append-only, authoritative record of every accepted
interaction and operational event (§4.2, §21.9).

**Namespace / scope** — The authorisation boundary (`user/*`, `agent/*`,
`project/*`, `workspace/*`, `organisation/*`, `system/governance`,
`public/reference`) that MUST be resolved before, not after, retrieval
ranking (§4.7, §9).

**OKF (Open Knowledge Format)** — The Git-backed, Markdown/YAML-based
format for approved, curated governing knowledge (§5, §26). Distinct from
the conversational journal, which is not automatically trusted knowledge.

**Outbox (transactional outbox)** — The pattern by which every durable
change requiring follow-up processing writes its job in the same
transaction as the change itself, enabling exactly-once-effective delivery
under retry (§14).

**Promotion** — The governed act of moving a memory candidate into a
protected shared scope; SHALL NOT be performed unilaterally by an agent
(§4.4, §16).

**Provenance** — The complete attribution chain for a memory: source,
creator, approving principal, evidence, authority, confidence, validity,
sensitivity, scope, status, version and supersession (§4.5).

**Route decision** — The immutable record explaining which capabilities
were selected, which were rejected, and why, for a given interaction
(§21.14). Every route decision SHALL be replayable.

**Superiority gate** — The measured, reproducible benchmark threshold a
native optimisation system (context optimisation, code intelligence,
execution proxy, control plane) MUST clear before it may be labelled
superior to a baseline (§4.13, §18.10, §19.13, §20.11, §21.18).

**Turn** — One complete unit of interaction lifecycle: message acceptance
through reply completion, checkpoint and post-turn consolidation (§11,
§21.3).
