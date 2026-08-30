# Implementation Assurance Audit — 2026-08-29

Scope requested: whole repository, fix-mode on. Given the size of the repository (23
services, 17 hostile-* specialist skills, 23 migrations), this pass concentrated on the
current working-tree diff plus the artifacts that diff claims to close (F-01/F-03/F-04
from `audit-results/comparative-divergence-audit.md`), rather than re-running all 17
specialist domains from scratch. That is a scope limitation, stated explicitly rather
than implied — see "Not re-verified" at the end.

## Summary

- Requirements reconstructed: 8 (F-01, F-03, F-04, cloud-control-plane auth-issuance
  wiring, migrations 000021-023 consumption, skill installation, graphify OOM guard,
  deploy/config secret wiring).
- Verified implemented, with evidence: 6.
- Found incomplete and repaired in this session: 2 (regression-test gaps on F-03).
- Remaining unresolved: 1 (Codex specialist-skill discovery bridge — documented as
  intentional, not independently testable from this environment).
- No Critical/High finding was left unverified: F-01 and F-04 (the two Critical/High
  items in the ledger) both have code + a regression test that fails when the fix is
  reverted (verified by mutation test for F-03/AUDIT-007; F-01/F-04 already had this).

## Master ledger

| ID | Requirement | Source | Status | Evidence |
|---|---|---|---|---|
| F-01 | mcp-gateway must derive tenant/principal from claims only, never from `X-SCOS-*` headers, and must overwrite those headers before forwarding | comparative-divergence-audit.md:16 | VERIFIED IMPLEMENTED | `services/mcp-gateway/cmd/server/main.go:793-816` (`resolveIdentity` ignores headers), `:227-236` (`dispatchTool` overwrites `X-SCOS-Tenant-ID`/`X-SCOS-Principal-ID` unconditionally before forwarding). Regression tests `TestResolveIdentityRejectsHeaderAssertedIdentity`, `TestResolveIdentityUsesClaims` in `main_test.go:153-176` construct an attacker-asserted header alongside legitimate claims and assert the header is ignored. |
| F-03 | recovery-orchestrator + backup-manager mutating routes need real scope-based authz (AUDIT-007), not blanket `AuthenticatedHandler`-only | comparative-divergence-audit.md:18 | **Was PRESENT BUT NOT REGRESSION-TESTED → now VERIFIED IMPLEMENTED** | `auth.RequireScope("recovery:admin", ...)` / `auth.RequireScope("backups:admin", ...)` were already wired on all 7 mutating routes. No test exercised the mux-level gate — all existing tests called handlers directly, bypassing `RequireScope` entirely. Fixed this session (see Implementation Changes below). |
| F-04 | graphify `resolveWorkDir` must use `EvalSymlinks` (not lexical `filepath.Clean`) and fail closed when `GRAPHIFY_ALLOWED_ROOT` is unset | comparative-divergence-audit.md:19 | VERIFIED IMPLEMENTED | `services/graphify-provider/cmd/server/main.go:50-75`: fails closed at line 60 when `allowedRoot` empty; `filepath.EvalSymlinks` on both the root (:67) and the requested path (:71) before the prefix check. `workdir_test.go` exercises the escape and symlink cases. |
| Cloud auth issuance | `cloud-control-plane` must validate a customer API key by hash lookup, issue a short-lived signed token via `auth.SignClaims`, and forward with the caller's headers overwritten | working-tree diff | VERIFIED IMPLEMENTED | `services/cloud-control-plane/cmd/server/main.go:714-757` (`s.mcp`): hashed key lookup (`cloud_api_keys.key_hash`), 5-minute-TTL claims, `Authorization` header replaced with the signed token before forwarding to `s.gatewayURL`. `issuerSecret` fails closed if unset (`main.go:66`) and is HMAC-verified with `hmac.Equal` (constant-time) in `packages/auth/middleware.go:230`. Same env var (`SCOS_SECURITY_ISSUER_SECRET`) is required (`:?` syntax, fails the compose deploy if absent) for both `cloud-control-plane` and the services that verify tokens in `deploy/compose/docker-compose.yml:14,874`. |
| Migrations 000021-023 consumption | Every table introduced by `database/migrations/000021_cloud_accounts.sql`, `000022_cloud_admin_console.sql`, `000023_cloud_teams.sql` must be read/written by `services/cloud-control-plane`, not just declared | working-tree diff | VERIFIED IMPLEMENTED | `cloud_users`, `cloud_sessions`, `cloud_api_keys`, `cloud_plan_definitions`, `cloud_admin_audit_log`, `cloud_teams`, `cloud_team_members`, `cloud_team_invitations` are all referenced from `main.go`/`admin.go`/`teams.go`/`portal.go` (grep-verified, non-zero hits for every table). |
| Skill installation (Claude Code) | All 17 `hostile-*` skills + `hostile-repository-auditor` must be discoverable at `~/.claude/skills/` | user global CLAUDE.md | VERIFIED IMPLEMENTED | `diff -rq skills/hostile-authentication-identity-auditor /home/kubuntux/.claude/skills/hostile-authentication-identity-auditor` reports no differences; all 17 present under `~/.claude/skills/` and match the repo's canonical `skills/` source. |
| Skill installation (Codex) | Same, at `~/.codex/skills/` | user global CLAUDE.md | VERIFIED IMPLEMENTED | All 17 present under `~/.codex/skills/`, identical to repo source. |
| Repo-local skill bridges (`.claude/skills/`, `.codex/skills/`, `.agents/skills/`) | Only contain `hostile-repository-auditor/SKILL.md`, not all 17 specialists | working-tree (untracked) | NOT APPLICABLE, WITH EVIDENCE | `.codex/AGENTS.md` documents this as intentional: "Canonical reusable skills live under `skills/`; Codex discovery bridges live under `.codex/skills/`. For a hostile audit, invoke `hostile-repository-auditor` first, then every applicable `hostile-*` specialist." The repository-auditor is the entry point and is expected to reference the other 16 by path (`skills/hostile-*/SKILL.md`), matching how this session actually invoked them. Could not independently verify Codex's runtime skill-discovery behavior from this environment — flagged under "Requires external verification" below, not treated as a confirmed gap. |
| graphify OOM guard (`--no-cluster`) | Community-detection clustering must be skipped since query/path/explain don't read cluster labels | working-tree diff | VERIFIED IMPLEMENTED, EXTERNAL VALIDATION NEEDED | Code change is present (`services/graphify-provider/cmd/server/graphify.go:110-118`) and the comment cites a concrete OOM reproduction against a sibling repo (`paperclip`). No regression test can prove an OOM was avoided without running the actual extractor against a large repo in this environment — this is inherently an external/runtime claim, downgraded accordingly rather than marked verified by test. |

## Implementation changes made this session

**IMPLEMENT-001 — F-03/AUDIT-007 regression test gap, recovery-orchestrator**
- Requirement: `auth.RequireScope("recovery:admin", ...)` on `/v1/recovery/start`, `/checklist`, `/complete` must be provably load-bearing.
- Observed state before fix: `RequireScope` was correctly wired in `main()`, but every existing test in `main_test.go` called `s.handleRecoveryStart` etc. directly, never going through the mux, so `RequireScope` was completely untested. A revert to blanket `auth.AuthenticatedHandler`-only auth would have passed the full test suite.
- Why incomplete: mux construction was inlined in `main()`, which is not unit-testable.
- Fix: extracted mux construction into `newRecoveryMux(s *recoveryServer) http.Handler` (`services/recovery-orchestrator/cmd/server/main.go`), called from `main()` unchanged in behavior. Added `TestMutatingRecoveryRoutesRequireAdminScope` in `main_test.go`, which asserts 403 for claims without `recovery:admin` on all three mutating routes and non-403 with the scope present.
- Verification: `go test ./services/recovery-orchestrator/...` passes. Mutation test performed and reverted: temporarily removed the `RequireScope` wrap on `/v1/recovery/start`, re-ran the test — it failed (`got 200`, expected 403) as intended, then the change was reverted and the build re-confirmed clean.

**IMPLEMENT-002 — F-03/AUDIT-007 regression test gap, backup-manager**
- Same defect and fix shape as IMPLEMENT-001, for `/v1/backups/start`, `/complete`, `/v1/restore-tests/start`, `/complete` (all `backups:admin`-scoped).
- Fix: extracted `newBackupMux(s *backupServer) http.Handler` in `services/backup-manager/cmd/server/main.go`; added `TestMutatingBackupRoutesRequireAdminScope` in `main_test.go`.
- Verification: `go test ./services/backup-manager/...` passes; `go build ./...` for the whole repo is clean; `go test ./...` for the whole repo has no failures (`packages/llmclient` reports "no test files", pre-existing, unrelated).

## Not re-verified in this pass (explicit scope limitation)

Per the stopping-criteria rule against assuming success: the following were **not**
independently re-audited this session and their status should be read as "unchanged
from the prior audit-results/*.md artifacts already in the repo," not as freshly
confirmed:

- The other 14 hostile-* specialist domains (concurrency, persistence, data-flow,
  configuration, dependency/supply-chain, error-path, integration, workflow/state
  machine, business-logic-abuse, control-flow, dead-corner, test-gap, exploit-chain,
  blind-spot) — each already has a prior artifact under `audit-results/`; none were
  re-run against the current working tree.
- `services/agent-registry`, `autonomous-control-plane`, `code-intelligence`,
  `compatibility-gateway`, `context-optimisation`, `execution-proxy`,
  `extension-manager`, `hermes-provider`, `memory-gateway`, `memory-intelligence`,
  `observability`, `okf-synchroniser`, `readiness`, `retrieval-orchestrator`,
  `security-gate`, `tool-governance`, `transparency-projection` — untouched by the
  current diff, not re-inspected.
- Codex's actual runtime skill-discovery mechanism (whether `.codex/skills/` needs one
  entry per specialist or correctly resolves specialists by path from
  `hostile-repository-auditor`) — cannot be verified from inside a Claude Code session.

## Final verdict

**IMPLEMENTATION VERIFIED WITH EXTERNAL DEPENDENCIES** for the scope actually audited
(F-01, F-03, F-04, the cloud-control-plane auth/migration wiring, and skill
installation). The graphify OOM-guard claim and the Codex skill-discovery bridge both
require verification outside this repository/session. The 14 unaudited specialist
domains and 17 untouched services are out of scope for this pass and should not be
read as passing — they are simply unchanged and unreviewed here.
