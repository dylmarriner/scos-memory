# Specialist audit artifact

Skill: hostile-control-flow-auditor
Agent: Codex coordinator
Scope: handler dispatch and parser/indexer branches
Files/components inspected: code-intelligence main/pghandlers/pgstore/scipimport/treesitter; retrieval; Graphify
Entry points inspected: route registration, v1/v2 code paths, graph query/path/explain
Important callers/callees: route → identity/validation → storage/subprocess → response
Workflows traced: v1 legacy in-memory vs v2 durable code store; Graphify cache hit/miss; SCIP/Tree-sitter fallback
Trust boundaries: fallback branches and error swallowing
Attacker-controlled inputs: malformed syntax and incomplete content
Invariants tested: Tree-sitter parse errors preserve file-level fallback; auth middleware wraps routes
Findings: v1 and v2 are materially different products; parse errors are intentionally swallowed, which can yield apparently successful partial indexes; v2 activation has no complete-tree branch
Suspicions requiring validation: callers may select weaker v1 path unintentionally
Tests/commands executed: source review; Go suite pass
Coverage gaps: runtime route selection and deployment defaults
Areas explicitly not reviewed: client behavior
Recommended follow-up: capability/version negotiation and fail-visible index quality reports
