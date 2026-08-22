---
name: hostile-repository-auditor
description: "Run a maximum-paranoia, attacker-input-first repository security and correctness audit with independent specialist passes, evidence artifacts, cross-review, fixes, and coverage reconciliation."
---

# Hostile Repository Auditor

## Purpose
Coordinate a repository-wide adversarial audit and leave a reproducible evidence trail. This is an execution skill: inspect the real repository, invoke every specialist named in `specialists/`, and write one artifact per specialist under `audit-results/`.

## Threat model
Assume an external user, tenant member, webhook sender, queue producer, compromised integration, operator, or malicious local input can reach every exposed or “internal” boundary. Start from attacker-controlled inputs and trace to authorization, persistence, filesystem, network, templates, queues, secrets, subprocesses, and destructive effects. Trust behavior, not names, comments, tests, or intended reachability. Prefer valid-input abuse, second-order effects, replay, races, stale state, partial failure, and exploit chains.

## Adversarial mindset
Assume every trust boundary can be reached and every clean label can be wrong. Require a concrete attacker-to-sink trace, actively falsify candidates, and keep independent specialist conclusions separate until reconciliation.

## Inputs consumed
Repository instructions, tracked-file inventory, source and generated boundaries, routes/RPC/CLI/events, schemas/migrations, deployment/configuration, tests, specialist artifacts, and prior coverage gaps.

## Audit phases
1. Inventory all tracked files, languages, services, entry points, schemas, migrations, configuration, deployment, tooling, tests, generated/vendor boundaries, and external integrations.
2. Reconstruct actors, trust boundaries, meaningful workflows, state machines, invariants, assumptions, and input-to-sink paths.
3. Invoke all specialists in `specialists/` with distinct scopes; require their compact artifacts using `references/artifact-template.md`.
4. Reconcile artifacts into `audit-results/coverage-ledger.md`, `workflow-matrix.md`, `invariant-register.md`, and `assumption-register.md`.
5. Independently run three passes: inputs-to-sinks, state/business logic, and darkest corners. Do not reuse clean labels as evidence.
6. Validate every candidate: exact location, reachability, preconditions, violated invariant, impact, mitigation search, falsification, reproduction, confidence. Never report speculation as confirmed.
7. Independently cross-review every Critical/High candidate with an appropriate specialist; record disagreement and resolution.
8. For confirmed issues, make the smallest safe root-cause fix, add regression tests, search equivalent patterns, run targeted and broader checks, retry the proof, and document residual risk.
9. Invoke the blind-spot specialist last, reconcile its artifact, update skills with generalized successful techniques, and verify persistence.

## Delegation rules
Use independent agents when available, with separate raw scopes and no leaked conclusions. Minimum scopes are architecture/attack surface, authentication, authorization, data flow, workflows/business logic, concurrency, persistence, API behavior, queues/workers, integrations, configuration/deployment, dependencies, errors/recovery, tests, dead corners, and exploit chains. If delegation is unavailable, execute each specialist sequentially yourself and label Agent accordingly; do not claim parallel independence.

## Search strategy
Inventory first with `git ls-files`/`rg`, discover entry points and sinks, then trace callers/callees, trust boundaries, persistence, errors, retries, configuration, tests, and alternate interfaces. Record exact paths and line ranges.

## Investigation procedure
Run inventory, input-to-sink mapping, workflow/state/invariant reconstruction, specialist scopes, finding validation and cross-review, three independent completeness passes, and the final blind-spot pass in that order. Write persistent artifacts at each phase.

## Questions to ask
## Required questions
What controls this value? Where did it originate? Who calls this? What happens twice, concurrently, halfway through, out of order, after failure/cancellation/deletion/migration, under another tenant or identity, and under another configuration?

## Coverage tracking
## Input inventory and coverage
Enumerate HTTP/RPC/GraphQL/CLI/events/webhooks/queues/workers/files/config/environment/database/serialized/external inputs. For each meaningful component record entry points, callers, callees, inputs, side effects, privilege, state, dependencies, tests, auditor, status, findings. A file opened is not coverage: mark complete only after inputs, outputs, callers/callees, auth, state, failure, retry, concurrency, configuration, and external effects are considered.

## Evidence requirements
## Evidence and finding standards
Every claim needs exact paths and line ranges where practical, commands or tests, and a trace from attacker capability to impact. Use Confirmed, High-confidence, Plausible/requires validation, Hardening opportunity, or Non-issue. Severity weighs impact, likelihood, reach, detectability, and recoverability; do not inflate. Search for protections on every alternate path and treat clean claims without sufficient artifact evidence as gaps.

## False-positive elimination procedure
For every candidate verify reachability, attacker prerequisites, exact sink, violated invariant, impact, alternate mitigations, and reproduction where practical. Downgrade or close candidates that cannot survive falsification.

## Cross-component checks
Compare service checks with adapters, workers, queues, database policies, migrations, caches, external responses, deployment defaults, and tests. Follow identity, tenant, state, and data across every boundary.

## Escalation conditions
Escalate Critical/High candidates, cross-tenant or privilege transitions, exploit chains, contradictory controls, and areas requiring live infrastructure or a second reviewer.

## Exploit chains
After individual validation, build a graph of realistic chains. For each chain record initial capability, steps, findings, intermediate privilege/data, final impact, conditions, reliability, detection likelihood, and chain-breaking mitigations. Do not combine merely theoretical issues.

## Completeness and cross-review
The three passes must state what each found that earlier work missed. Critical/High review must attempt to disprove reachability, attacker prerequisites, mitigations, alternate paths, impact, and severity. Record unresolved disagreements, unreviewed components, and evidence limitations explicitly.

## Fix workflow
Do not change production behavior for unconfirmed suspicions. For confirmed findings fix the root cause minimally, add regression coverage, search equivalent vulnerable patterns, run focused then relevant broad checks, attempt bypass variants, and preserve unrelated work. Existing dirty changes are user-owned.

## Required artifacts and reporting
Write every specialist artifact from `references/artifact-template.md`, a master matrix, coverage ledger, workflow matrix, invariant and assumption registers, findings, exploit chains, fix log, three-pass results, cross-review record, blind-spot result, and skill improvement log under `audit-results/`. The final report must include honest metrics and exact persistent skill paths.

## Skill-artifact requirements
Each specialist invocation must leave a compact persistent artifact even when clean, including inspected files/components, entry points, callers/callees, workflows, trust boundaries, inputs, invariants, findings, suspicions, commands, gaps, exclusions, and follow-up. The coordinator must read and reconcile each artifact.

## Required output artifact
The final report plus specialist artifacts, coverage ledger, workflow matrix, invariant register, assumption register, findings, exploit chains, fix log, cross-review record, completeness passes, blind-spot result, and skill-improvement log under `audit-results/`.

## Self-improvement procedure
After the blind-spot pass, identify techniques that found issues or eliminated false positives, generalize them beyond this repository, update the owning persistent skill, and append a dated entry to `audit-results/skill-improvement-log.md`. Never embed secrets or repository-only trivia. Re-run skill validation after edits.

## Stopping criteria
Stop only after all specialists have artifacts, three independent passes and blind-spot review are recorded, all Critical/High candidates are cross-reviewed, confirmed fixes are verified or explicitly blocked, coverage gaps are named, and persistence is checked. If environment limits execution, report the exact limitation and downgrade claims rather than assuming success.
