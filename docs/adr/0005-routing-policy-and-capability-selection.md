# ADR-0005: Deterministic constraints gate learned capability ranking

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §21.6–§21.8 and §4.16 require routing to first remove
prohibited/unavailable capabilities deterministically, and only rank the
survivors with learned models. §35 requires a dedicated ADR for route
policy and capability-registry contract changes.

## Decision

Every routing decision goes through `packages/routing-contracts` types
(`IngressEvent` → `IntentClassification` → `CapabilityRegistration`
lookup → `RouteDecision` → `ExecutionPlan`/`ExecutionStep`) in this fixed
order:

1. Hard filter: identity, authority, data classification, side-effect
   class and health remove ineligible `CapabilityRegistration` entries.
2. Only the surviving set is ranked (expected success, correctness,
   latency, cost, historical performance).
3. The chosen `RouteDecision` records `policy_version` and
   `registry_generation` so it is replayable against exactly the inputs
   that produced it.

A learned ranking model MAY reorder the surviving set. It MAY NOT
reintroduce a capability the deterministic filter already excluded — the
filter and the ranker are separate stages and the ranker never sees
excluded candidates.

## Consequences

- A routing bug in the ranking model can misorder good options but cannot
  grant access to a prohibited one.
- Every decision is explainable without re-running the ranker, because the
  filter's reasoning is recorded independently of the ranking score.

## Compatibility

`RouteDecision` and `CapabilityRegistration` fields are additive-only.
Existing `policy_version` values remain valid decision references even
after new fields are added.

## Migration

Registry updates happen via `registry_generation` increments, not
in-place edits — a new generation is a new set of rows, so historical
`RouteDecision` records keep resolving against the generation they cite.

## Rollback

Reverting `policy_version` to a prior value is sufficient; because the
filter/rank split means the deterministic stage is policy-independent
where possible, most rollbacks only affect ranking behaviour, not
authorization behaviour.

## Index rebuild

The capability registry is rebuilt from live health checks and deployment
state, never hand-edited (§21.6) — a rebuild produces a new
`registry_generation`, it does not mutate history.

## Verification

Failure injection `autonomy_conflicting_route_labels`,
`autonomy_missing_capability_health`, and
`autonomy_route-policy_rollback` cases exercise this filter/rank
separation directly. `route_replay_success` (§21.17) is the standing
metric that a decision can be reproduced from its recorded inputs.
