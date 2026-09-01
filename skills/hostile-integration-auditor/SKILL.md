---
name: hostile-integration-auditor
description: "Audit APIs, webhooks, callbacks, brokers, queues, signatures, replay protection, retries, parsing, trust assumptions, delayed/duplicate responses, and external failure."
---

# hostile-integration-auditor

## Purpose
Audit APIs, webhooks, callbacks, brokers, queues, signatures, replay protection, retries, parsing, trust assumptions, delayed/duplicate responses, and external failure.

## Threat model
Assume attacker-controlled or externally influenced data can reach every exposed, internal, asynchronous, persisted, operational, and compatibility boundary. Treat cross-tenant and privilege-changing effects as security-sensitive.

## Adversarial mindset
Do not trust names, comments, tests, intended reachability, or prior clean labels. Prefer valid-input abuse, alternate interfaces, replay, second-order effects, races, stale state, and partial failure.

## Inputs consumed
HTTP clients/servers, webhook handlers, queue payloads, adapters, third-party config, signature and timeout code.

## Scope
All relevant hand-written code plus security-relevant generated, vendored, schema, migration, deployment, and test boundaries. Read applicable repository instructions and preserve unrelated changes.

## Search strategy
Start with tracked-file inventory and repository references, then use symbol/reference search, route/handler discovery, dangerous-sink search, schema/config inspection, and test mapping. Keep exact paths and line ranges.

## Investigation procedure
Trace request/response trust boundaries; test absent/invalid signatures, replay, duplicate, timeout, malformed, oversized, and contradictory external responses. Record evidence in audit-results/hostile-integration-auditor.md using skills/hostile-repository-auditor/references/artifact-template.md. Inspect callers and callees, success and failure, configuration, persistence, retries, and alternate paths before calling a component clean.

## Questions to ask
What does the system accept from an external party without proving origin, freshness, scope, or consistency?

## Cross-component checks
Compare application checks with DB/RLS, adapters, workers, queues, migrations, external responses, caches, and deployment defaults. Follow identity, tenant, state, and attacker data across every boundary.

## Evidence requirements
Cite exact locations, reachable entry point, attacker preconditions, trace, violated invariant, impact, mitigation search, commands/tests, and confidence. Separate confirmed, high-confidence, plausible, hardening, and non-issue.

## False-positive elimination procedure
Attempt to disprove reachability; search for centralized and alternate protections; verify the claimed sink and impact; test a bypass variant; downgrade or close when a real mitigation blocks the path.

## Coverage tracking
List files/components, entry points, callers/callees, workflows, inputs, trust boundaries, invariants, tests, gaps, and exclusions in the artifact. File opening alone is not coverage.

## Escalation conditions
Escalate to the master auditor for suspected Critical/High impact, cross-tenant or privilege escalation, exploitable chains, contradictory controls, or evidence requiring a second specialist.

## Stopping criteria
Stop only after the assigned scope, alternate interfaces, error/retry/concurrency paths, configuration and persistence effects, and tests have been considered, with gaps explicitly recorded.

## Required output artifact
A persistent audit-results/hostile-integration-auditor.md artifact containing every field in the shared template, plus findings and commands with results.

## Self-improvement notes
Record any search, heuristic, false-positive lesson, invariant, or cross-component pattern that materially improved detection. Generalize it and propose the owning skill update in audit-results/skill-improvement-log.md.

