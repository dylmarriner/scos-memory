# Specialist audit artifact

Skill: hostile-dead-corner-auditor
Agent: Codex coordinator
Scope: legacy v1 code store, fallback adapters, unused graph metadata, operational corners
Files/components inspected: v1/v2 code paths, language adapters, Graphify cache, dashboard/integrations
Entry points inspected: legacy routes and fallback branches
Important callers/callees: v1 handler → in-memory maps; parse failure → file symbol; graph cache → child process
Workflows traced: users selecting v1; unsupported language; stale/empty graph; disabled embeddings
Trust boundaries: compatibility paths and “internal” routes
Attacker-controlled inputs: route choice, unsupported language/content, cache timing
Invariants tested: explicit fallback behavior
Findings: v1 remains a separate in-memory implementation beside v2; unsupported/parse-error paths can appear successful with reduced coverage; dead/legacy paths are not covered by durable persistence guarantees
Suspicions requiring validation: whether any clients still route to v1
Tests/commands executed: source route inventory; Go suite pass
Coverage gaps: production traffic route distribution
Areas explicitly not reviewed: client telemetry
Recommended follow-up: deprecate or clearly label v1 and expose index quality/capability metadata
