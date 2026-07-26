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
