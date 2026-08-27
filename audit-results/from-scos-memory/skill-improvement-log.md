# Skill improvement log

2026-08-23: generalized heuristic — whenever a service stores results keyed by a
caller-supplied identifier, audit the read/list/restore endpoints together with
the write path. A correct write-side mutex or state machine does not establish
principal ownership or prevent cross-request overwrite.

2026-08-23: generalized heuristic — treat “authenticated headers” as untrusted
unless the same repository proves issuer verification, transport binding, and
claim-to-object authorization. Header/body equality is validation, not identity.

2026-08-23 | hostile-configuration-auditor | Compose topology changed internal reachability. Added generalized host-binding and network-topology review.

2026-08-23 | hostile-authorization-auditor | Header equality is not authentication. Added a generalized issuer-proof rule for every service boundary.

2026-08-23 | hostile-workflow-state-machine-auditor | Caller-chosen IDs enabled overwrite/replay analysis. Added a generalized ownership test for stateful endpoints.
