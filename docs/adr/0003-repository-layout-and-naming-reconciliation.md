# ADR 0003: Repository layout and naming reconciliation

## Status

Accepted. Documents existing, intentional drift between `GOVERNANCE.md`'s
descriptive component names and the actual repository layout, so that an
implementation audit does not need to re-flag these as unexplained gaps.

## Context

An independent governance audit compared the working tree against
`GOVERNANCE.md` and found several places where the implementation is real
and functioning but named or structured differently from the document's
illustrative names:

- `GOVERNANCE.md` §20 refers to the "Native Adaptive Execution Proxy"; the
  implementing service is `services/execution-proxy`. No functional gap:
  the shorter directory name was chosen for consistency with the other
  `-manager`/`-gateway`/`-orchestrator` service names in `services/`.
- `GOVERNANCE.md` §25 describes a single "Transparency Dashboard"
  component. The implementation splits this into a backend,
  `services/transparency-projection` (the permission-filtered read-model
  aggregator described in §7.14), and a separate `dashboard/` directory
  holding the client-facing static asset. This split follows the same
  backend/frontend separation used elsewhere in the repository rather than
  being an oversight.
- `GOVERNANCE.md` §13 lists a `dead_letter_jobs` table. The implemented
  table is `outbox_dead_letters` (`database/migrations/000003_control_plane.sql`).
  Functionally it is the dead-letter table for `outbox_jobs` required by
  §14; the name follows the repository's `outbox_*` grouping convention
  instead of the document's literal name.
- The root-level `goverance.md` (misspelled) is an intentional compatibility
  stub, not a stray duplicate: it exists only to redirect readers who typo
  the filename to the authoritative `GOVERNANCE.md`, and explicitly states
  that policy must not be added there.

## Decision

These four naming differences are accepted as-is. They do not represent
missing functionality, and renaming working, tested, RLS-covered code to
match the document's illustrative names would be pure churn. Future
implementation audits should treat naming drift documented here as
resolved, and should flag *new* naming drift only when it obscures which
component satisfies which governance section.

## Consequences

- `GOVERNANCE.md` itself is not edited to match implementation naming: it
  remains the normative document and is allowed to use descriptive names
  that don't have to equal directory or table names one-for-one.
- Any future rename of `execution-proxy`, `transparency-projection`,
  `dashboard`, or `outbox_dead_letters` should update or supersede this ADR.

## Compatibility

Not applicable in the schema/wire sense: this ADR changes no running
contract, only which names are treated as satisfying which governance
section. Nothing that talks to `services/execution-proxy` or the
`outbox_dead_letters` table needs to change.

## Migration

None required. No table, API path, or service name changes as a result of
this ADR — it documents existing names as accepted rather than renaming
anything.

## Rollback

If a future decision supersedes this ADR (e.g. to actually rename
`execution-proxy` to match `GOVERNANCE.md`'s illustrative name), that
decision is its own ADR referencing this one; there is no rollback state
for this ADR itself since it changes no code.

## Index rebuild

Not applicable — no index, schema, or derived-projection shape changes.

## Verification

`scripts/audit_governance.py` cross-references `GOVERNANCE.md` §37's
repository layout against the actual tree; this ADR is the recorded
justification the audit script's naming-drift allowlist points to. Any
new drift not covered by this ADR's four documented cases should still be
flagged by the structural audit.
