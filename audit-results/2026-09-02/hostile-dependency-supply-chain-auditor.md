# Specialist audit artifact

Skill: hostile-dependency-supply-chain-auditor
Agent: Codex coordinator
Scope: Go/Python/Node dependencies and executable integrations
Files/components inspected: `go.mod`, worker `pyproject.toml`, package manifests, Dockerfiles, Graphify subprocess boundary
Entry points inspected: build/runtime dependency loading
Important callers/callees: services → pgx/tree-sitter/SCIP; provider → `graphify`
Workflows traced: build, parser execution, LLM/embedding calls
Trust boundaries: lockfiles/images/external binary
Attacker-controlled inputs: dependency/build configuration and subprocess output
Invariants tested: direct argv and pinned Go dependency versions
Findings: capability relies on external Graphify binary and multiple language grammars; SBOM/scanning is documented but no fresh CI run was performed in this audit
Suspicions requiring validation: binary provenance/version pin and Python lock reproducibility
Tests/commands executed: manifest review; Go suite pass
Coverage gaps: vulnerability scan/SBOM/signature verification
Areas explicitly not reviewed: registry state
Recommended follow-up: pin/verify Graphify artifact and execute CI supply-chain gates
