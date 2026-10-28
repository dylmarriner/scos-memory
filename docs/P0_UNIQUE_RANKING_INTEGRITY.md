# P0 unique-evidence ranking integrity

The P0 benchmark's deterministic ranking metrics previously rewarded duplicate result IDs.

- precision_at_k divided distinct relevant hits by distinct returned IDs. Three slots [a, a, b] with two relevant documents were incorrectly scored as 1.0 rather than 2/3.
- ndcg gave each occurrence of a relevant ID another gain. Repeating one relevant document [a, a] yielded 1.6309, above the normalized maximum of 1.0.

Precision now uses the number of returned ranking slots, and each relevant ID contributes at most one gain. nDCG also rewards each source ID only on its first appearance; duplicates still consume rank positions.

Three RED-before-fix and GREEN-after-fix regression tests cover precision, nDCG, and the complete per-case metric bundle. The frozen seven-case P4 lexical baseline was requalified because the harness source changed; its rankings, scores, and forbidden-evidence counts are unchanged. Only the recorded harness SHA and Git base SHA changed.

This is benchmark measurement integrity, not live BGE model-quality evidence, production latency qualification, or completion of all P0–P8 gates.
