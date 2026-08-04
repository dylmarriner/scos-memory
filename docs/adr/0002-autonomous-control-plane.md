# ADR 0002: Autonomous Cognitive Control Plane

## Status

Accepted by governing blueprint version 1.2.0.

## Decision

All supported client, agent, model and tool interaction paths shall pass through a native Autonomous Cognitive Control Plane.

The control plane owns journalling, identity and scope resolution, task classification, capability planning, automatic memory and code retrieval, context compilation, tool-loop mediation, reply persistence, checkpointing, post-turn consolidation, event-triggered maintenance and recovery.

Agents are not required to know that the memory system exists or call a memory tool for normal continuity.

## Rationale

Agent-directed memory calls are unreliable because prompts change, models vary, tool awareness degrades and crashes occur outside the agent's control. Continuity and routing are infrastructure responsibilities.

## Safety boundary

Autonomous routing does not expand authority. Confirmation-gated and high-impact actions remain confirmation-gated. Deterministic policy removes prohibited options before learned ranking.

## Consequences

- Direct model and tool credentials remain behind the control plane.
- Every route decision is durable, explainable and replayable.
- Capability contracts and health become first-class platform data.
- All adapters must emit the same canonical ingress envelope.
- Agents become replaceable workers rather than owners of durable state.

## Compatibility

Every ingress adapter (HTTP, SDK, MCP host, A2A, CLI, IDE, scheduled
workflow, event/filesystem trigger, recovery replay) must translate into
the same canonical ingress event before submission (GOVERNANCE.md §21.2).
Adapters are additive: a new adapter cannot change the shape the control
plane already commits to the journal, only add a new producer of it.

## Migration

The control plane's route-decision and capability-registry schemas
(`route_decisions`, `capability_graph` tables) evolve additively per
GOVERNANCE.md §12. A capability registry generation bump
(`registry_generation`) is the migration unit for routing policy changes —
old and new generations can coexist so in-flight routes keep replaying
against the policy version they were decided under.

## Rollback

Route-policy and capability-registry changes are versioned
(`policy_version`, `registry_generation`) and promoted through the
GOVERNANCE.md §21.15 sequence (offline replay → adversarial tests → shadow
→ canary → rollback validation). Rollback is reverting to the previous
`policy_version`/`registry_generation` value; because every decision
record pins the version it was made under, no historical decision becomes
unexplainable after a rollback.

## Index rebuild

The capability registry is rebuilt from live health checks, deployment
manifests and verified outcome telemetry (GOVERNANCE.md §21.6) — it is a
derived projection, never hand-edited authoritative state. A full rebuild
re-registers every service's declared capability record and re-runs health
checks; no route decision history is lost because decisions reference the
registry generation, not live registry rows.

## Verification

Autonomy acceptance is gated on the §21.18 thresholds (100% ingress
interception, 100% pre-dispatch journalling, zero direct-database bypass,
≥98% routing precision/recall on the frozen benchmark suite). Failure
injection (`tests/failure-injection`) covers direct-path bypass attempts,
missing capability health, conflicting route labels, and interrupted route
recovery.
