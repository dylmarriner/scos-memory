# Specialist audit artifact

Skill: hostile-data-flow-auditor
Agent: Codex coordinator
Scope: request/evidence/code data to storage and retrieval
Files/components inspected: MCP gateway, memory/retrieval services, code/Graphify providers, migrations
Entry points inspected: JSON bodies, headers, file content, Graphify output, embeddings
Important callers/callees: gateway → services; parser → symbol rows; Graphify → memory facts/relations
Workflows traced: raw message → candidate → memory version; source file → symbol/edge; graph → fact/relation
Trust boundaries: external LLM, parser output, subprocess output, SQL
Attacker-controlled inputs: content, hashes, repo paths, graph labels/edges
Invariants tested: max request bodies and direct argv usage
Findings: Graphify ingestion collapses rich edge metadata to two relation types and does not persist snapshot/source location in the memory relation payload; code content is not embedded in the v2 path
Suspicions requiring validation: sensitive source leakage through downstream context
Tests/commands executed: source review; Go suite pass
Coverage gaps: end-to-end redaction and derived-index deletion
Areas explicitly not reviewed: production logs
Recommended follow-up: canonical evidence IDs and typed edge payloads with provenance/time
