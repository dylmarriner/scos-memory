# ADR-0008: Execution proxy rewrites are allowlisted, versioned, and raw-preserving

- **Status:** Accepted
- **Date:** 2026-07-29
- **Decision owner:** Dylan Marriner

## Context

GOVERNANCE.md §20.3–§20.4 requires that command rewrites are allowlisted,
provably semantics-preserving, and never discard exit status or raw
output. §35 requires a dedicated ADR for proxy rewrite policy.

## Decision

`services/execution-proxy` represents a rewrite as a `RewriteRule` applied
to produce a `CommandPlan`, executed to an `ExecutionResult` that always
retains a `rawArtifact`/`rawRestorationMetadata` pair alongside the
`ResultGroup` (grouped/reduced view). Every mutating or unknown-risk
operation goes through `toolGovernanceClient` (`GovernedOperation`) before
execution — the proxy has a `rejectingGovernance` fallback that refuses to
execute rather than silently bypassing governance when the tool-governance
service is unreachable (§20.12: the proxy optimizes representation, it
does not grant authority).

Unknown commands default to transparent pass-through with raw capture,
never speculative rewriting (§20.6). Required safety modes
(`bypass`/`observe`/`safe`/`adaptive`/`forensic`) are selected per
operation, with high-impact and incident-response operations defaulting to
`forensic` or `bypass` (§20.9).

The reduced view is delivered only when it is smaller than the raw output
(§20.5 step 10). In the reduced view stdout is grouped, exact repeated
lines on either stream are carried once with their occurrence counts
(`reduction.repeated_lines`), and each stderr line is delivered once:
diagnostic lines under `errors`/`warnings`, the rest in `stderr_summary`.
When that would not be smaller than stdout plus stderr, the raw streams are
passed through unchanged with no `groups`, `errors` or `warnings`, and
`reduction` reports `applied: false`, `reduction_ratio: 0` and the reason.

## Consequences

- A rewrite bug can produce a worse summary but cannot silently hide exit
  status, signals, or the ability to recover the exact raw output.
- The `rejectingGovernance` fail-closed default means a tool-governance
  outage degrades to "no execution" rather than "ungoverned execution."

## Compatibility

`RewriteRule` and `ExecutionResult` fields are additive. New command
adapters register new `supported_command_patterns` without changing the
shape existing rules and results use.

## Migration

Rewrite rules are versioned data (`proxy_policy_version` on every
`ExecutionResult`), not code deployed in place — a new rule version is
added, not edited, so historical results keep citing the version that
actually ran.

## Rollback

Per §20.8's promotion sequence (observation → policy candidate → offline
replay → adversarial/failure testing → shadow → canary → measured
comparison → promotion/rejection), rollback is reverting
`proxy_policy_version` to the last approved value. The proxy never learns
by editing its live rules in place (§20.8), so rollback is always a
version pointer change, never a data repair.

## Index rebuild

Not applicable in the search-index sense; the durable artifact is the raw
result store, which is authoritative and never rebuilt from a derived
summary — summaries are rebuilt from raw results, never the reverse.

## Verification

Failure injection `execution_proxy_*` category (parser uncertainty, hidden
error output, changed exit status, unsafe rewrite, duplicate side effects)
is the standing regression suite. The §20.11 superiority gate (≥70% median
token reduction, 100% exit-status/signal preservation, 100% raw-result
restoration, no increase in unsafe execution) is the acceptance bar.
