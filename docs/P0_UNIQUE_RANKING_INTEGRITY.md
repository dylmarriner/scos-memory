# P0 unique-evidence ranking integrity

The P0 benchmark's deterministic ranking metrics previously rewarded duplicate result IDs.

- precision_at_k divided distinct relevant hits by distinct returned IDs. Three slots [a, a, b] with two relevant documents were incorrectly scored as 1.0 rather than 2/3.
- ndcg gave each occurrence of a relevant ID another gain. Repeating one relevant document [a, a] yielded 1.6309, above the normalized maximum of 1.0.

Precision now uses the number of returned ranking slots, and each relevant ID contributes at most one gain. nDCG also rewards each source ID only on its first appearance; duplicates still consume rank positions.

Three RED-before-fix and GREEN-after-fix regression tests cover precision, nDCG, and the complete per-case metric bundle. The frozen seven-case P4 lexical baseline was requalified because the harness source changed; its rankings, scores, and forbidden-evidence counts are unchanged. Only the recorded harness SHA and Git base SHA changed.

This is benchmark measurement integrity, not live BGE model-quality evidence, production latency qualification, or completion of all P0–P8 gates.

## Authority-score duplicate hardening (2026-10-29)

The optional `authority_accuracy` metric still counted every repeated
appearance of an authoritative document as another authority hit. An
adversarial ranking `[a, a, x]`, with only `a` authoritative, incorrectly
scored 2/3 instead of 1/3. Authority now counts **unique authoritative
source IDs** in the top-k slots, while every repeated slot remains in the
denominator. Two unit regressions first reproduced 2/3 (RED) through both
the metric and `compute_scores`, then verified 1/3 (GREEN). The two-authority
case `[a, a, b]` scores 2/3, not 3/3.

The seven-case frozen P4 lexical smoke baseline was requalified because the
shared harness digest changed. All seven rankings, measured scores and
forbidden-hit counts remained byte-for-byte identical; only the recorded
`harness_sha256` and `git_base_sha` metadata changed. This is scoring
integrity, **not** a claim of deployed retrieval accuracy, general model
quality, latency qualification, production rollout or §41 completion.
