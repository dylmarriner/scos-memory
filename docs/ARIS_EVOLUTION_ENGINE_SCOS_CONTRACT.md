# SCOS Memory × ARIS Evolution Engine: Evidence and Context Contract

**Status:** Proposed integration contract, 29 October 2026. No new executable authority or implementation claim.
**Canonical parent:** [ARIS-EE-BP-001](https://github.com/dylmarriner/ARIS/blob/main/docs/evolution/ARIS_EVOLUTION_ENGINE_TECHNICAL_BLUEPRINT_AND_ROADMAP.md).
**Repository authority:** [SCOS GOVERNANCE.md](../GOVERNANCE.md), [SCOS P0–P8 roadmap](SCOS_P0_P8_ROADMAP.md), existing [governed-learning ADR](adr/0009-governed-learning-and-promotion.md).

## Responsibility boundary

SCOS owns durable task/observation/provenance storage, evidence retrieval, consolidation and versioned learned knowledge. ARIS Harness owns task orchestration and the evolution proposer; ARIS OS independently enforces owner-scoped privileged effects; ARIS Intelligence owns model artifacts. **SCOS does not authenticate or issue TEST/DEPLOY grants**, modify evaluation pass criteria, or directly promote executable changes into production.

## Proposed evidence data contract

For each task/experiment/candidate, record immutable links or content digests for:
- originating task and authorization scope; run/trace identity and time;
- actor, model/provider/version, adapter/tool versions, context/skill versions;
- tool invocation, observed output, verified artifact, environment and error;
- baseline and candidate commit/digest, test protocol/evaluator version and fixture IDs;
- independently recorded success/failure, uncertainty, confidence and regression data;
- resource costs, retries, rejected attempts, repair lineage and rollback target;
- owner decision **references** and decision proof digests, never executable permission tokens.

The raw history is evidence, not an automatically verified claim. Keep original source linked and queryable; summarize for retrieval without overwriting or losing raw records. Enforce tenant/namespace/sensitivity visibility before ranking and returning evidence. A derived playbook must cite its supporting traces, classification status, contradictions and supersession lineage.

## Context and skill evolution

- **ACE-style increments:** store discrete, revisioned playbook deltas and independently evaluate retrieval usefulness. Never fully rewrite the sole durable knowledge record on each iteration.
- **MCE-style meta-skills:** separately version context-construction procedures and the context artifacts they produce; compare across fixed replay tasks and context-token budgets.
- **GEPA / prompt search:** SCOS supplies provenance-linked retrieval/traces to the proposer; Harness owns generation, candidate selection and comparison.
- **Preserve adverse findings:** failed experiments, context collapse, prompt regressions and high-cost candidates remain searchable and cannot be silently converted to positive evidence.
- **SCOS self-modification:** may be proposed like any other component, but candidate cannot change the canonical SCOS governance record, protected evaluator, authorization verifier or sealed test fixtures inside its isolated experiment.

## Delivery across evolution phases

| Phase | SCOS deliverable | Acceptance evidence |
|---|---|---|
| EE-P0 | Inventory existing SCOS schema, ingestion, indexes and P0–P8 status | Source/CI-linked gap report; no guessed completion |
| EE-P1 | Normalized trace/candidate/experiment provenance links | Crash recovery, idempotency, scope isolation and raw-record integrity |
| EE-P2 | Independent evaluator result ingestion without candidate-controlled score mutation | Digest/fixture mismatch and tampering tests |
| EE-P3 | Store audit references for distinct TEST and DEPLOY decisions | No authorization derived from memory content alone |
| EE-P4 | Queryable failure families and candidate lineage | Trace-grounded findings and failed-attempt retention |
| EE-P5 | Incremental playbooks and context-assembly evaluation | No destructive collapse; held-out relevance, token and privacy checks |
| EE-P6–P9 | Versioned meta-skills, Pareto history, rollback evidence and operating runbooks | Reproducible claims and evidence recovery under fault injection |

## Compatibility and status

This document supplements, and does **not** supersede, existing SCOS governance §4.12, §16, §20.8, ADR-0009 or the canonical SCOS P0–P8 roadmap. **EE-P0–EE-P9 numbering is a separate ARIS Evolution Engine workstream.** Until code and CI verify these contracts, all interfaces are proposed.
