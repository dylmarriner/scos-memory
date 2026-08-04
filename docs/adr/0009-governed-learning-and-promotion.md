# ADR-0009: Learned changes promote through offline replay, canary, and rollback — never in place

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §4.12 (bounded evolution) and §21.15 require that
self-learning components select among approved policies and may propose
new rules, but may not rewrite governing constraints or deploy unreviewed
executable code. §16 requires human approval for governing policy,
security, permission-expansion and high-impact promotion decisions. §35
requires a dedicated ADR for learning-signal changes.

## Decision

Two distinct learning surfaces exist and both follow the same promotion
sequence:

1. **Memory promotion** — `workers/consolidation/src/scos_consolidation/promoter.py`'s
   `evaluate_promotion` returns a `PromotionDecision`, not a direct write.
   Memory classes in `HUMAN_REVIEW_CLASSES`, and semantic candidates above
   a sensitivity threshold, are routed to human review rather than
   auto-promoted (§16). No candidate becomes authoritative memory by
   consolidation alone.
2. **Policy/ranking learning** (routing, context optimisation, code
   intelligence, execution proxy) — every candidate change is versioned
   data (`policy_version`, `proxy_policy_version`, `registry_generation`,
   `compiler_version`) and passes: observation → policy candidate →
   offline replay → adversarial/failure testing → shadow execution →
   limited canary → measured comparison → approved promotion or rejection
   (§20.8, §21.15). Outcome labels generated solely by the model being
   evaluated are not sufficient evidence of improvement (§21.15).

Neither surface may write to a protected shared scope, expand its own
permissions, or deploy unreviewed executable code (§4.4, §4.12).

## Consequences

- A learning bug can produce a bad policy candidate, but it cannot reach
  production without clearing offline replay, adversarial testing, and
  canary — the blast radius of a bad candidate is bounded by those gates.
- Human reviewers see a `PromotionDecision`/policy candidate with its
  evidence, not a silent state mutation.

## Compatibility

`PromotionDecision` and policy-candidate records are additive; new
learning signal types (§21.15's list: task success, human correction,
agent retry, fallback use, retrieval expansion, test/build results,
citation accuracy, tool failure, latency, token use, cost, recovery
outcome, security denial) can be added to the evidence a candidate carries
without changing how existing candidates are interpreted.

## Migration

None at the schema level for existing decisions — a new promotion rule or
learning signal is a new evaluation path, not a rewrite of
`evaluate_promotion`'s existing branches.

## Rollback

Rollback is reverting the relevant `*_version`/`*_generation` pointer, per
ADR-0005/0006/0007/0008's per-subsystem rollback sections. Memory
promotion has an additional rollback: superseded, revoked or corrected
memory (§4.6) rather than deleted history.

## Index rebuild

Not applicable directly — this ADR governs decision *process*, not a
storage index. Where a promoted memory feeds an index (embeddings,
full-text), that index follows the normal derived/rebuildable rule
(§4.1).

## Verification

`workers/consolidation/tests` covers promotion-decision branches
including human-review routing. §33 requires automatic promotion or
learned-policy deployment to be narrowed or disabled when unsupported
facts, sensitive misclassification, high duplicate rate, weak conflict
detection, instruction loss, diagnostic loss, restoration failure,
benchmark regression, or excessive human rejection appear — this is the
standing kill switch for every learning surface described here.
