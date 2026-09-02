# SCOS Competitive Audit Implementation Plan

## Source of Truth
`SCOS_MEMORY_COMPETITIVE_AUDIT_AND_MASTER_PLAN.md`, audited 2026-09-01.

The implementation follows its dependency order, beginning with P0.9 baseline benchmarks and then the code-index/snapshot work that remains after the current branch's recent durable-store, parser, SCIP, lexical-search, and Graphify changes.

## First Tranche: Deployed Benchmark Baseline

1. Extend the benchmark runner with a Linode/deployed mode whose base URL, dedicated tenant/repository identifiers, and bearer/API credential come only from environment variables or the existing secret mechanism. Never commit endpoint or credentials.
2. Keep the existing local deterministic runner as an offline unit baseline, but label it separately from deployed evidence. A deployed run must fail closed on unavailable endpoints, authentication failures, malformed responses, or missing required metadata; it must never substitute fixtures and claim a live pass.
3. Seed and clean up only the dedicated benchmark tenant/repository. Exercise read-only queries plus the minimum isolated writes needed to create snapshots/index fixtures; exclude destructive production operations and never use user data.
4. Add machine-readable benchmark records containing source SHA, schema/index versions, deployment endpoint identity (redacted/non-secret), tenant/repository fixture IDs, configuration/model metadata, per-case latency, and failure reasons.
5. Implement the first acceptance corpus from the audit: exact symbol resolution, one-line edit isolation, rename lineage, deletion tombstone, branch divergence, failed-index non-activation, retry idempotency, Graphify extractor/config generation separation, and cross-tenant non-disclosure. Compare results against the current deployed baseline before changing behavior.

## Acceptance Criteria
- Linode runs are isolated to a dedicated benchmark tenant/repository and do not mutate production data.
- Live failures are explicit and non-passing; no fixture fallback is hidden behind the deployed mode.
- Results are reproducible, machine-readable, and include sufficient non-secret metadata to identify the deployment and code/index generation.
- Appendix C cases are automated or explicitly marked blocked by a missing deployed capability.
- Existing local benchmark, Go, Python, validation, and audit gates remain green.

## Constraints
- Do not begin P1 retrieval or moat features before the baseline exposes current quality and correctness.
- Do not weaken durability, tenant isolation, or confirmation controls to make benchmarks pass.
- Preserve existing uncommitted Graphify changes and do not modify `GOVERNANCE.md`.

## Follow-on Dependency Order
1. Reconcile benchmark failures with P0.3-P0.6: durable code persistence, immutable snapshots, SCIP, and Tree-sitter fallback.
2. Close P0.7-P0.8: snapshot/config-addressed Graphify artifacts and resumable idempotent indexing jobs.
3. Add release parity, tenant-isolation matrix, and derived-deletion ledger only after the evidence/snapshot contracts are measured.

## Validation
- Offline: existing `benchmarks/run_acceptance.py` and benchmark unit tests.
- Deployed: explicit Linode benchmark command against the dedicated tenant, with response/schema validation and no silent fallback.
- Repository: `make fmt`, `make test`, `make validate`, and `make audit`, plus focused code-intelligence and Graphify tests.
