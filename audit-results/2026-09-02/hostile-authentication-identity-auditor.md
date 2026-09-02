# Specialist audit artifact

Skill: hostile-authentication-identity-auditor
Agent: Codex coordinator
Scope: `packages/auth`, service wrappers, identity propagation to memory/code/Graphify
Files/components inspected: `packages/auth/middleware.go`; code-intelligence `requireTenant`; retrieval/memory resolvers; Graphify forwarding
Entry points inspected: bearer middleware, control-plane wrapper, direct downstream handlers
Important callers/callees: `AuthenticatedHandler`/`ControlPlaneHandler` → handlers → header/body identity resolution
Workflows traced: claims token vs static service token; gateway forwarding; downstream fallback headers
Trust boundaries: issuer secret, static internal token, control-plane token, caller headers
Attacker-controlled inputs: Authorization, X-SCOS headers, JSON tenant/principal
Invariants tested: claims take precedence; body identity must match claims where implemented
Findings: static service-token path still relies on caller/gateway headers; no `jti`, audience, issuer, or revocation evidence found in middleware
Suspicions requiring validation: whether deployment topology makes fallback headers unreachable to untrusted callers
Tests/commands executed: source review; existing auth/tenant tests; full Go suite pass
Coverage gaps: live token replay/revocation and service identity rotation
Areas explicitly not reviewed: external IdP
Recommended follow-up: bind service credentials to tenant/service identity or use mTLS plus signed identity envelope
