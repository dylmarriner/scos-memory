#!/usr/bin/env python3
"""Fail-closed evidence gate for GOVERNANCE.md §41 "Definition of done".

This script does not re-implement or re-prove the underlying behaviour that
§41 requires (that belongs to the owning services' own test suites). It
mechanically checks, per DoD bullet, whether the specific test evidence that
would justify claiming the bullet is satisfied still exists in the tree —
by grepping for the exact test function names cited in this script — and it
fails loudly, per item, when that evidence is missing, renamed, or deleted.

It also parses the master table row for section 41 in
``docs/governance-audit-current.md`` and refuses to report an overall PASS
unless that row itself says PASS, because several DoD bullets are owned by
services outside any single pass's assigned scope (§22 hook coverage, §32
metric breadth, retrieval/OKF/ACP ingress work, etc. — see "What Remains
Open" in that document). This prevents the gate from being quietly flipped
to green by deleting or weakening the audit doc's own carried-work note
without the underlying work actually landing (§42 prohibits fabricated
"healthy" state).

Usage: python3 scripts/verify_definition_of_done.py
Exit code 0 only if every checkable DoD item has live evidence AND the audit
doc's own §41 row says PASS. Non-zero otherwise (this is expected today —
see docs/governance-audit-current.md line 104).
"""
from __future__ import annotations

import re
import sys
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


@dataclass
class DoDItem:
    bullet: str
    file: str
    pattern: str
    note: str
    in_scope: bool = True


def function_exists(path: Path, pattern: str) -> bool:
    if not path.is_file():
        return False
    text = path.read_text(encoding="utf-8", errors="replace")
    return re.search(pattern, text, re.MULTILINE) is not None


ITEMS: list[DoDItem] = [
    DoDItem(
        bullet="Accepted messages and completed responses are durably committed.",
        file="services/autonomous-control-plane/cmd/server/main_test.go",
        pattern=r"^func TestAgentSuccessIsReturnedOnlyAfterAuthoritativeCommit\(",
        note="ACP only reports agent success after the authoritative commit; "
             "see also TestAgentLifecycleCapturesReplyAndCommitsBeforeReturn "
             "in the same file, and services/memory-gateway/cmd/server/outbox_test.go "
             "TestOutboxHandlersGuardOnReadiness for the durable-outbox path "
             "(commit is synchronous, outbox delivery is the async leg).",
    ),
    DoDItem(
        bullet="Interrupted streams can be reconstructed.",
        file="services/recovery-orchestrator/cmd/server/main_test.go",
        pattern=r"^func TestInterruptedTurns\(",
        note="recovery-orchestrator tracks and reports InterruptedTurns/OpenOperations "
             "as part of its recovery checklist.",
    ),
    DoDItem(
        bullet="Tools have durable state, idempotency and reconciliation.",
        file="services/tool-governance/cmd/server/main_test.go",
        pattern=r"^func TestMutationRequiresIdempotencyKey\(",
        note="tool-governance rejects tool mutations without an idempotency key; "
             "see also services/execution-proxy/cmd/server/main_test.go "
             "TestExecuteUnknownSideEffectIsReconciled for reconciliation.",
    ),
    DoDItem(
        bullet="Checkpoints restore sessions.",
        file="services/recovery-orchestrator/cmd/server/main_test.go",
        pattern=r"^func TestApplyFindingsResolvesOKFCheckpointsAndLeases\(",
        note="recovery-orchestrator resolves checkpoints and leases during recovery; "
             "see also services/autonomous-control-plane/cmd/server/main_test.go "
             "TestAgentLifecycleDispatchesGovernedHookStages (on_checkpoint hook).",
    ),
    DoDItem(
        bullet="Memory is namespace-scoped and RLS-tested.",
        file="database/integration/rls_test.go",
        pattern=r"^func TestGovernanceRLSAndIsolation\(",
        note="cross-tenant/cross-namespace isolation and per-table RLS enablement "
             "are asserted against a live PostgreSQL fixture.",
    ),
    DoDItem(
        bullet="Full-text and vector retrieval are combined.",
        file="services/retrieval-orchestrator/cmd/server/main_test.go",
        pattern=r"^func TestVectorLiteralBuildsPgvectorParameter\(",
        note="retrieval-orchestrator is excluded from this workstream's edit scope; "
             "citing existing evidence only, not re-verifying it.",
        in_scope=False,
    ),
    DoDItem(
        bullet="Retrieval returns provenance and conflicts.",
        file="services/retrieval-orchestrator/cmd/server/main_test.go",
        pattern=r"^func TestDecodeResultJSONLoadsEvidenceAndConflicts\(",
        note="retrieval-orchestrator is excluded from this workstream's edit scope; "
             "docs/governance-audit-current.md carries this as owned elsewhere.",
        in_scope=False,
    ),
    DoDItem(
        bullet="Corrections preserve versions.",
        file="workers/consolidation/tests/test_promoter.py",
        pattern=r"^    def test_build_memory_version_contains_version_1\(",
        note="promoter builds a versioned memory record (version 1, content hash) "
             "on every promotion; see also test_build_memory_version_hashes_content.",
    ),
    DoDItem(
        bullet="Promotion is governed.",
        file="workers/consolidation/tests/test_promoter.py",
        pattern=r"^    def test_review_governing_class\(",
        note="promoter routes governing-class, revocation-class, sensitive-semantic "
             "and high-impact candidates to review rather than auto-promoting them.",
    ),
    DoDItem(
        bullet="OKF is Git-backed.",
        file="services/okf-synchroniser/cmd/server/main_test.go",
        pattern=r"^func TestGitCommitReadsRepositoryHead\(",
        note="services/okf-synchroniser is excluded from this workstream's edit scope; "
             "citing existing evidence only, not re-verifying it.",
        in_scope=False,
    ),
    DoDItem(
        bullet="Search indexes rebuild from authority.",
        file="services/code-intelligence/cmd/server/index_equivalence_integration_test.go",
        pattern=r"^func TestPGIncrementalSnapshotEquivalentToCleanRebuild\(",
        note="live PostgreSQL test compares incremental and clean full-source indexing "
             "of the same target for symbol versions, chunks and call edges; "
             "this is not proof of a production authority-to-index replay.",
    ),
    DoDItem(
        bullet="Backups have been restored successfully.",
        file="services/backup-manager/cmd/server/backup_integration_test.go",
        pattern=r"^func TestBackupAndDrillsAgainstPostgreSQL\(",
        note="integration-tagged PostgreSQL test takes an encrypted disposable "
             "backup and exercises real archive, key-table and full schema "
             "restore drills; it does not prove live/off-host recovery or PITR.",
    ),
    DoDItem(
        bullet="Failure injection passes.",
        file="tests/failure-injection/runner.py",
        pattern=r"def run",
        note="see scripts/audit_governance_runtime.py:failure_injection_skips for "
             "the mechanical check that this runner cannot silently report "
             "'skipped' in place of an executed result.",
    ),
    DoDItem(
        bullet="Agents and clients enter through the Autonomous Cognitive Control "
               "Plane rather than direct model, tool or database paths.",
        file="services/autonomous-control-plane/cmd/server/main_test.go",
        pattern=r"^func TestIngestValidatesAuthenticatedIdentityAndJSON\(",
        note="ACP validates authenticated identity on its ingress path. This is "
             "necessary but not sufficient evidence: no test in the tree currently "
             "asserts that OTHER services (memory-gateway, execution-proxy, "
             "tool-governance) refuse direct unauthenticated-of-ACP traffic. "
             "Recorded here as PARTIAL evidence, not a full mandatory-ingress proof.",
    ),
    DoDItem(
        bullet="Agents with no memory awareness retain full continuity across "
               "turns and crashes.",
        file="services/compatibility-gateway/cmd/server/main_test.go",
        pattern=r"^func TestAgentAdapterContractCoverage\(",
        note="compatibility-gateway asserts adapter contract coverage and "
             "compatibility-tier completeness; see also "
             "TestSupportedIntegrationBoundariesComplete and "
             "TestUntestedBoundariesLabelledExperimental in the same file. This "
             "proves the compatibility boundary is declared and tested, not that "
             "crash-continuity itself is exercised end-to-end.",
    ),
]


def check_items() -> tuple[list[str], list[str]]:
    """Return (failures, partial_or_carried_notes)."""
    failures: list[str] = []
    carried: list[str] = []
    for item in ITEMS:
        path = ROOT / item.file
        found = function_exists(path, item.pattern)
        label = "OUT-OF-SCOPE-CITED" if not item.in_scope else "IN-SCOPE"
        if not found:
            failures.append(
                f"[{label}] MISSING EVIDENCE — \"{item.bullet}\"\n"
                f"    expected test matching {item.pattern!r} in {item.file}\n"
                f"    ({item.note})"
            )
        else:
            carried.append(
                f"[{label}] evidence present — \"{item.bullet}\"\n"
                f"    {item.file} :: {item.pattern}\n"
                f"    ({item.note})"
            )
    return failures, carried


def audit_doc_gate() -> list[str]:
    """Refuse to report overall PASS unless the audit doc's own §41 row says PASS."""
    doc = ROOT / "docs" / "governance-audit-current.md"
    if not doc.is_file():
        return ["docs/governance-audit-current.md is missing; cannot confirm §41 gate status"]
    text = doc.read_text(encoding="utf-8", errors="replace")
    match = re.search(r"^\|\s*41\s*\|\s*Definition of done\s*\|\s*([^\|]+)\|", text, re.MULTILINE)
    if not match:
        return ["docs/governance-audit-current.md has no master-table row for section 41"]
    status = match.group(1).strip()
    if not status.upper().startswith("PASS"):
        return [
            f"docs/governance-audit-current.md master table reports §41 as "
            f"\"{status}\", not PASS — the definition-of-done gate cannot be "
            f"considered closed while the audit doc itself carries this open. "
            f"See that document's \"What Remains Open\" section for the specific "
            f"blocking items (§22 hook coverage, §32 metric-emission breadth, "
            f"§25 integration evidence, and the other Carried sections)."
        ]
    return []


def main() -> int:
    item_failures, evidence_present = check_items()
    doc_failures = audit_doc_gate()

    print("=== Definition-of-Done evidence check (per §41 bullet) ===")
    for line in evidence_present:
        print(f"PASS  {line}")
    for line in item_failures:
        print(f"FAIL  {line}")

    print()
    print("=== §41 master-table gate (docs/governance-audit-current.md) ===")
    if doc_failures:
        for line in doc_failures:
            print(f"FAIL  {line}")
    else:
        print("PASS  master table reports §41 as PASS")

    all_failures = item_failures + doc_failures
    print()
    if all_failures:
        print(f"DEFINITION OF DONE GATE: FAIL ({len(all_failures)} blocking finding(s))")
        return 1
    print("DEFINITION OF DONE GATE: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
