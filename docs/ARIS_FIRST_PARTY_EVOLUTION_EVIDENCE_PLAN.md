# SCOS: First-Party Evolution and Native-Model Training Evidence Plan

**Status:** Proposed ARIS-specific memory integration, 31 October 2026.  
**Authority:** [SCOS GOVERNANCE](../GOVERNANCE.md), [canonical P0–P8 roadmap](SCOS_P0_P8_ROADMAP.md), [governed learning ADR](adr/0009-governed-learning-and-promotion.md) and [ARIS first-party engineering policy](https://github.com/dylmarriner/ARIS/blob/main/docs/evolution/FIRST_PARTY_EVOLUTION_TRAINING_BLUEPRINT.md).

## Responsibility

SCOS is the durable **evidence and provenance plane** for the ARIS Evolution Engine and ARIS Intelligence training, not a second optimizer, inference runtime, model trainer, owner-approval service, or trusted grader. Its records must remain usable if every upstream research repository disappears. It does not import GEPA, DSPy, Darwinian Evolver, Hermes Self-Evolution, Atropos or Axolotl as a required service or Python dependency.

## Required event and data contracts

Preserve native SCOS event scope, identity and journal semantics; normalize these *references*, not copies of huge model weights:

- **Task and candidate history:** immutable source task ID, verified outcome, trace artifact digest, model/skill/context versions, mutation hypothesis, parent candidate digest and tested patch.
- **Rollout history:** environment version/digest, seed, policy and tokenizer digests, episode ID, ordered actions/observations and terminal verified outcome. Never accept model-generated narration as an authoritative tool result.
- **Reward evidence:** scorer code/config digest, expected-result evidence reference, dimensional reward/penalty scores, measurement method, uncertainty and fraud/leakage flags. A scorer result stored by SCOS is **not** the independent evaluation authority.
- **Training datasets:** scope and source provenance, user data consent/eligibility, retention/deletion requirements, source license, redaction status, train/validation/held-out split IDs and contamination checks.
- **Training runs:** base model checkpoint and tokenizer, immutable recipe/data/hardware digests, resource cost, run status and checkpoint artifacts; no model weight blobs in ordinary retrieved memory.
- **Decision references:** owner TEST/DEPLOY grant **IDs or cryptographic digests only**, not token secrets and never authorization semantics derived from a retrieved memory.
- **Champion/release records:** baseline and challenger performance, regression families, release identity, rollback target, previous champion, rejection reasons and reproducibility evidence.

Existing `schemas/events/event-envelope.schema.json` and SCOS state journals stay authoritative. Cross-repo schemas are versioned and negotiated rather than duplicating another untracked event envelope. Where a new projection is needed it must derive from exact immutable journal entries. Keep source and derived records distinguishable.

## Incremental learning and data protection

Adapt the **concepts** behind ACE and MCE into SCOS-owned context and meta-skill revisions:
1. Record a small evidence-backed lesson as a new versioned delta; retain original evidence and contradictory cases. No blanket full rewrite of durable history.
2. Store retrieval quality and token costs separately from task completion; context compression alone is not proof of an improvement.
3. Prevent source-private or sensitive-user data from reaching public training corpora or third-party APIs without eligibility, consent, scope checks and an audit trail.
4. Enforce deletion, correction and provenance-supersession requirements across derived datasets and pending training queues; note that trained weights may require separate remediation/evaluation.
5. Train/val/held-out contamination detection must use raw source identity plus exact/near-duplicate checks rather than relying on random shuffling alone.
6. Preserve failed experiments and negative evidence; avoid rewarding repetition from duplicated source documents or self-produced success summaries.
7. External research implementations are not entrusted with memory retention, promotion or legal/privacy rules.

## Proposed SCOS-owned interfaces

- `record_experience(envelope, source_artifact_digest) -> durable_event_id`: source/scope verified and idempotent.
- `record_candidate_evidence(candidate_digest, trace_refs, hypothesis) -> evidence_link`: no actual proposal execution.
- `record_rollout/record_reward`: versioned documents validating episode identity and signer/scorer provenance. Trust attribution, not self-scored acceptance.
- `register_dataset_manifest`: validates consent/classification, split and leakage proofs before any data export to a separately approved training environment.
- `query_failure_cases(goal, evaluation_version, scope, budget)`: returns ranked IDs and raw evidence pointers with source lineage; Harness composes reasoning context.
- `record_training_release`: records exact model/checkpoint and independent evaluation receipts only, not deployment grants.

These are *logical API contracts* to reconcile with real SCOS services before creating new endpoints.

## Roadmap and verification

| Existing SCOS milestone | ARIS first-party addition | Required test |
|---|---|---|
| P0–P3 / M0–M3 | Baseline identity, journal lifecycle, idempotent source-linked tasks and episodes | Crash/restart, exact suite identity, tenant isolation |
| P4 / M4–M5 | Evidence-based failure retrieval and training-eligibility references | Duplicate provenance never counts twice; cross-scope leak denied |
| P5–P6 / M6 | Bounded immutable reward/rollout/dataset projections | Invalid scorer digest, episode split contamination and bad identity rejected |
| P7 / M7–M8 | Incremental playbook/meta-skill revisions, contradictory evidence and negative results | No destructive rewrite, no silent review/promotion of protected information |
| P8 / M9 | Event integrity, offline rebuild, retention/correction/deletion coordination | Cold-replay equals authoritative journal; export/revoke and rollback receipts survive outages |

**EE workstream:** EE-P0 inventory, EE-P1 evidence, EE-P2 protected grader ingestion, EE-P3 owner decision *reference*, EE-P4 failure mining, EE-P5 playbooks, EE-P6–P9 experiment lineage/recovery. These do not change or complete SCOS P0–P8 or M0–M9 status.

## Primary acceptance cases

1. Identical task/reward evidence delivered twice is not double counted; same ID with different digest is rejected.
2. A model-provided string claiming success cannot replace independently verified tool output.
3. A private transcript cannot be exported as a public training dataset through inferred consent.
4. Missing/expired data consent prevents new exports, with appropriate withdrawal remediation traces.
5. Candidate attempts to change evaluation scores, fixture identity, grant interpretation or source classification are detected.
6. Replay/rebuild restores failed candidate lineage and previous champion reference without altering immutable source.
7. Missing model-training services do not impair SCOS retrieval, journal durability or ARIS OS offline operation.
8. No dependency reference to the three permanently excluded independent repositories is introduced.

## Source relevance

Atropos contributed the concept of environment rollouts and scored trajectories; Axolotl contributed checkpoint/training lifecycle needs; GEPA and Darwinian search contributed failure-linked candidate histories. All are research inputs only. The implementation, governance and memory authority remain SCOS-owned. No upstream source code has been copied into this document.
