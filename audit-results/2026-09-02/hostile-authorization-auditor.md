# Specialist audit artifact

Skill: hostile-authorization-auditor
Agent: Codex coordinator
Scope: tenant/namespace/repository/object authorization
Files/components inspected: database RLS policies; retrieval namespace resolution; code-intelligence v2 handlers; Graphify/memory forwarding
Entry points inspected: `/v2/code/*`, `/v1/retrieve`, memory facts/relations, Graphify APIs
Important callers/callees: handlers → repository ownership checks/namespace membership → SQL
Workflows traced: authenticated tenant selection, repository query/index, memory namespace search
Trust boundaries: application checks vs SQL RLS vs service headers
Attacker-controlled inputs: repository IDs, branch, namespace names, tenant fields
Invariants tested: cross-tenant IDs rejected in v2 handler; namespace membership required in retrieval
Findings: repository ownership is checked in application code, but RLS runtime enforcement is unproven because no FORCE RLS/runtime role creation was found; Graphify relation forwarding preserves tenant/principal headers rather than a cryptographically bound envelope
Suspicions requiring validation: direct access to internal services in deployed topology
Tests/commands executed: policy/source review; existing tenant tests; Go suite pass
Coverage gaps: live cross-tenant integration through every endpoint
Areas explicitly not reviewed: production IAM
Recommended follow-up: create non-bypass app roles, FORCE RLS, and endpoint×principal negative tests
