# Specialist audit artifact

Skill: hostile-integration-auditor
Agent: Codex coordinator
Scope: agent integrations, Graphify, embeddings, SCIP, external services
Files/components inspected: `integrations/`, Graphify provider, embeddings worker, MCP adapters
Entry points inspected: MCP tools and internal HTTP calls
Important callers/callees: agent → MCP gateway → service mesh; Graphify → memory-intelligence
Workflows traced: agent setup, code query, graph query, embedding request
Trust boundaries: external agent/client, internal HTTP, external LLM/indexer artifacts
Attacker-controlled inputs: MCP arguments, service URLs/config, graph output
Invariants tested: direct subprocess argv and body limits
Findings: integration breadth is real but runtime support is not equivalent to configuration presence; Graphify and code-intelligence produce separate, weakly unified representations
Suspicions requiring validation: every advertised integration against live clients
Tests/commands executed: repository inventory; Go suite pass
Coverage gaps: live agent contract tests
Areas explicitly not reviewed: third-party hosted connectors
Recommended follow-up: integration conformance suite with real MCP calls and provenance assertions
