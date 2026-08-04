---
title: Tools
description: Approved native services and the tool-shaped capabilities they expose to agents through the control plane.
tags: [tools, capabilities, services]
status: active
---

# Tools

GOVERNANCE.md §4.19 and §21.6 require that a capability's presence in this
inventory, not an agent's voluntary tool choice, is what makes it usable.
Every entry below is a native SCOS service registered in the capability
registry (`packages/capability-registry`) and reachable only through the
Autonomous Cognitive Control Plane — no agent receives direct credentials
to any of these.

## Native services

| Service | Purpose | GOVERNANCE.md |
|---|---|---|
| `memory-gateway` | Sole entry point for persistent memory operations: auth, policy, schema validation, idempotency, turn management, correction | §7.1 |
| `autonomous-control-plane` | Mandatory ingress, capability planning, lifecycle coordination, routing, recovery | §7.10, §21 |
| `retrieval-orchestrator` | Hybrid lexical/full-text/vector/relational retrieval with RRF fusion and namespace authorisation | §7.3, §17 |
| `context-optimisation` | Reversible context capsule compilation, budgeting, restoration | §7.7, §18 |
| `code-intelligence` | Repository-scale exact/symbolic/structural/relational/semantic/historical code understanding | §7.8, §19 |
| `execution-proxy` | Deterministic command/tool-output reduction with raw restoration handles | §7.9, §20 |
| `extension-manager` | Signed plugin/skill/hook/adapter lifecycle: install, enable, disable, rollback, uninstall | §7.11, §22 |
| `compatibility-gateway` | Agent boundary negotiation and compatibility tier reporting | §7.12, §23 |
| `hermes-provider` | First-class Hermes `MemoryProvider` lifecycle implementation | §24 |
| `memory-intelligence` | Retain/recall/reflect, entity modelling, temporal graph, beliefs, diary | §7.13, §25 |
| `transparency-projection` | Permission-filtered real-time dashboard read models | §7.14, §25.9 |
| `okf-synchroniser` | Git-backed OKF validation, indexing, divergence detection | §7.5, §26 |
| `security-gate` | Signed bearer issuance, principal/tenant authentication | §27 |
| `tool-governance` | Tool state machine, idempotency, reconciliation | §28 |
| `agent-registry` | Agent registration, capability declaration, handoff packets | §29 |
| `recovery-orchestrator` | Startup readiness, interrupted-turn discovery, lease reset | §30 |
| `backup-manager` | Backup artifacts, manifests, restore verification, retention | §31 |
| `observability` | Durable telemetry store, correlation IDs, OTLP export | §32 |

## Explicit agent-facing tools

Per GOVERNANCE.md §21.13 and §24.5, agents are never required to call these
for baseline continuity — they exist for inspection and correction only:

```text
scos_recall
scos_explain
scos_user_model
scos_memory_graph
scos_correct
scos_forget
scos_status
```

## Capability contract

Every tool above registers a versioned `capability_id` record (§21.6) that
declares its input/output schema, required permissions, side-effect class,
confirmation policy, idempotency support, and health state. Routing SHALL
exclude unhealthy, unauthorised, or stale capabilities before ranking
(§21.8) — a tool's presence in this document is necessary but not
sufficient for it to be selected at runtime.
