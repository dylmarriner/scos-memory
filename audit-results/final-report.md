# Hostile Repository Audit — 2026-08-23

## 1. Executive summary

This is a multi-service Go/Python/PostgreSQL repository with 18 HTTP servers,
171 registered routes, Docker Compose deployment, migrations/RLS, workers,
adapters, and 256 Go tests plus 72 Python test functions. The dominant risk is
that identity is represented by caller-provided headers or JSON fields without a
repository-wide issuer-verification boundary.

Seven findings were identified and are now remediated in source and
deployment: the original five (AUDIT-001..005), plus two discovered on a
follow-up pass that specifically chased the report's own flagged gaps —
AUDIT-006 (tool-governance had scope checks but no tenant-ownership binding)
and AUDIT-007 (recovery-orchestrator and backup-manager had no scope
requirement at all on mutating routes). The highest-risk path
is execution-proxy shell execution combined with spoofable identity and
unauthorized result access. Compose host bindings were hardened to loopback in
the existing working change; service authentication and secret enforcement are
now implemented. No live deployment or external-system exploit was run, so
runtime/topology-dependent verification remains outstanding. Execution-proxy's
shell-execution capability itself remains intentional and unrestricted — it
is this service's core product function, not an oversight — and any decision
to allowlist or remove it is a product-scope decision left to the repository
owner, not made unilaterally by this audit.

## 2. Architecture and attack surface

Trust boundaries are network-to-service, headers/JSON-to-identity, database-to-
worker, service-to-service, container-to-host, extension-manifest-to-capability,
and command-to-shell. Inputs include HTTP headers and bodies, CLI/adapters,
environment variables, persisted rows, migrations/RLS settings, manifests,
queue-shaped records, external responses, and filesystem contents. Sinks include
PostgreSQL writes, subprocess execution, raw output files, HTTP clients, in-memory
authority/control-plane state, and capability registration.

## 3. Skills created and used

The durable canonical skills are under `skills/`; Claude Code and Codex discover
them through the repository bridges. All 17 were loaded and used sequentially
because no delegation runtime was exposed in this session.

| Skill | Claude Code location | Codex location | Used |
|---|---|---|---|
| hostile-repository-auditor | `.claude/skills/hostile-repository-auditor/SKILL.md` | `.codex/skills/hostile-repository-auditor/SKILL.md` | yes |
| hostile-control-flow-auditor | `skills/hostile-control-flow-auditor/SKILL.md` | same canonical path | yes |
| hostile-data-flow-auditor | `skills/hostile-data-flow-auditor/SKILL.md` | same canonical path | yes |
| hostile-authorization-auditor | `skills/hostile-authorization-auditor/SKILL.md` | same canonical path | yes |
| hostile-authentication-identity-auditor | `skills/hostile-authentication-identity-auditor/SKILL.md` | same canonical path | yes |
| hostile-workflow-state-machine-auditor | `skills/hostile-workflow-state-machine-auditor/SKILL.md` | same canonical path | yes |
| hostile-business-logic-abuse-auditor | `skills/hostile-business-logic-abuse-auditor/SKILL.md` | same canonical path | yes |
| hostile-concurrency-auditor | `skills/hostile-concurrency-auditor/SKILL.md` | same canonical path | yes |
| hostile-error-path-auditor | `skills/hostile-error-path-auditor/SKILL.md` | same canonical path | yes |
| hostile-configuration-auditor | `skills/hostile-configuration-auditor/SKILL.md` | same canonical path | yes |
| hostile-persistence-auditor | `skills/hostile-persistence-auditor/SKILL.md` | same canonical path | yes |
| hostile-integration-auditor | `skills/hostile-integration-auditor/SKILL.md` | same canonical path | yes |
| hostile-dependency-supply-chain-auditor | `skills/hostile-dependency-supply-chain-auditor/SKILL.md` | same canonical path | yes |
| hostile-test-gap-auditor | `skills/hostile-test-gap-auditor/SKILL.md` | same canonical path | yes |
| hostile-exploit-chain-auditor | `skills/hostile-exploit-chain-auditor/SKILL.md` | same canonical path | yes |
| hostile-dead-corner-auditor | `skills/hostile-dead-corner-auditor/SKILL.md` | same canonical path | yes |
| hostile-blind-spot-auditor | `skills/hostile-blind-spot-auditor/SKILL.md` | same canonical path | yes, last |

Each canonical skill explicitly defines purpose, threat model, adversarial
mindset, inputs, search/investigation, questions, evidence, falsification,
cross-component checks, stopping/escalation, output, coverage, and self-
improvement. `.claude/CLAUDE.md` and `.codex/AGENTS.md` preserve discovery.

For every specialist row marked “same canonical path”, the exact path is
`skills/<skill-name>/SKILL.md`; both Claude Code and Codex use that canonical
path through their repository instruction bridges.

## 4. Skill files changed/created

Created/present in this audit worktree: the 17 canonical files listed above,
`.claude/CLAUDE.md`, `.codex/AGENTS.md`, and both repository bridge skill files.
Persistent audit artifacts are under `audit-results/`.

## 5. Coverage report

181 tracked files inventoried; 69 Go; 24 Python; 9 SQL; 599 Go functions/methods;
171 routes; 18 muxes; 256 Go tests; 72 Python test functions; 21 Dockerfiles;
6 migration/policy surfaces; 16 specialist artifacts plus blind-spot; 0 live
integration deployments. Meaningful review covered service handlers, adapters,
workers, database policies/migrations, deployment, tests, and operational/legacy
boundaries. Unreviewed or unresolved: live PostgreSQL/RLS behavior, production
topology, real queue/broker execution, external identity provider behavior, and
exhaustive function-level semantic coverage.

## 6. Workflow matrix

| Workflow | Entry points | Main state/effects | Result |
|---|---|---|---|
| Execution | `/v1/plan`, `/v1/execute`, restore/results | command → shell → result/raw file | AUDIT-001, AUDIT-004 |
| Authority | security-gate `/v1/auth/*` | JSON identity → in-memory token | AUDIT-002 |
| Tool governance | register/transition/start/complete/reconcile | proposed → authorised → executing → terminal | AUDIT-003, AUDIT-006 |
| Extension install | `/v1/install` and aliases | manifest → enabled hooks/skills/capabilities | AUDIT-003 |
| Recovery/backup | recovery and backup start/complete routes | operator state and verification metadata | AUDIT-007 |
| Memory/data plane | gateway, retrieval, intelligence, RLS | tenant/principal headers → SQL context | live integration gap |
| Async workers | consolidation/index/policy workers | rows/events → derived state | retry/concurrency test gaps |

## 7. Findings

### AUDIT-001 — Execution proxy trusts caller-controlled identity before shell execution

Severity: High candidate. Confidence: High source confidence. Status: fixed and verified.
Category: authentication/authorization/command execution.

Affected: `packages/service-auth/auth.go` and
`services/execution-proxy/cmd/server/main.go`. All non-health routes now require
a verified HMAC bearer token; legacy identity headers are overwritten from
verified claims before handlers run. Shell execution and request-controlled
working directories remain intentional capabilities requiring future allowlisting.

### AUDIT-002 — Security gate issues authority without authenticating the issuer

Severity: High candidate. Confidence: High. Status: fixed and verified.
Category: authentication/authorization. `services/security-gate/cmd/server/main.go:113-142,154-203`
`/v1/auth/issue` now requires `X-SCOS-Issuer-Secret`, a non-placeholder
deployment secret, a tenant, bounded one-hour expiry, and produces a random
HMAC-signed bearer token. Regression tests cover missing issuer and validation.

### AUDIT-003 — Control-plane mutators lack visible ownership/authentication

Severity: High candidate. Confidence: High. Status: fixed and verified.
Category: authorization/workflow. Tool registration/transition/operation
completion/reconciliation are exposed at `services/tool-governance/cmd/server/main.go:109-120,161-217,244-328`
without principal, tenant, or role checks. Shared bearer middleware now rejects
unauthenticated callers, and extension installation additionally verifies an HMAC
manifest signature bound to the canonical manifest and content hash. Durable
principal-bound transitions remain a follow-up for a database-backed control plane.

### AUDIT-004 — Execution results and raw output are not identity-bound

Severity: High candidate. Confidence: High. Status: fixed and verified.
Category: authorization/data disclosure/business logic. `/v1/results/{id}` and
`/v1/results` return stored results without auth (`375-423`); raw restore reads
any valid 64-hex handle (`391-412`). Results now store owner tenant/principal;
list, detail, and restore reads require the matching verified identity; and
operation IDs are server-owned random identifiers rather than caller-selected
map keys.

### AUDIT-005 — Default database password remains `change-me`

Severity: Medium. Confidence: Confirmed. Status: fixed and verified. Compose now
binds published ports to loopback and fails closed when `POSTGRES_PASSWORD`,
`SCOS_AUTH_ISSUER_SECRET`, or `SCOS_EXTENSION_SIGNING_SECRET` is absent.

### AUDIT-006 — Tool-governance authorization checks scope but not tenant ownership

Severity: High candidate. Confidence: High. Status: fixed and verified.
Category: authorization/tenant isolation (IDOR/BOLA), addresses the "durable
principal-bound transitions" follow-up left open under AUDIT-003.
Affected: `services/tool-governance/cmd/server/main.go`. Every mutating route
required a scope (`tools:admin`, `operations:execute`, `operations:complete`,
`operations:reconcile`) but never compared the caller's tenant to the
resource's owning tenant, and list/get endpoints returned every tenant's
tools and operations to any authenticated caller. A tenant-b caller holding
`tools:admin` could transition tenant-a's tools; a caller holding
`operations:execute`/`complete`/`reconcile` could start operations against
another tenant's authorised tool and complete or reconcile another tenant's
operations. `ToolDefinition` and `ToolOperation` now carry `Tenant`/`Owner`
stamped from the verified bearer identity at registration/start time, and
every handler enforces `identity.Tenant == resource.Tenant` (an "admin" scope
remains an intentional cross-tenant escape hatch, matching existing
`serviceauth.HasScope` semantics elsewhere). List endpoints filter to the
caller's tenant. Regression tests
(`TestToolGovernanceTenantIsolation`, `TestToolGovernanceAdminScopeCrossesTenants`)
exercise the real HTTP handlers through `serviceauth.Require` with two
distinct signed tenants and assert 404 on every cross-tenant access attempt.
Residual risk: state remains in-memory with no durable, restart-surviving
audit trail of who performed each transition — the database-backed
control-plane half of the original follow-up is still open.

### AUDIT-007 — Recovery and backup operator actions had no scope requirement

Severity: High candidate. Confidence: High. Status: fixed and verified.
Category: authorization, resolves the "authorization gap" flagged against
the Recovery/backup row of the workflow matrix (§6).
Affected: `services/recovery-orchestrator/cmd/server/main.go`,
`services/backup-manager/cmd/server/main.go`. Both services required only a
valid bearer token (`serviceauth.Require`) on every route, including the
mutating ones — unlike tool-governance and extension-manager, which already
gated equivalent operator actions with `serviceauth.RequireScope`. Any
authenticated caller could force a service's recovery checklist to
"completed" without any step running, or mark a backup/restore-test
"completed"/"passed" without one ever having run, corrupting operational
verification state used to judge whether a service actually recovered or a
backup is actually restorable. `/v1/recovery/start`, `/v1/recovery/checklist`,
`/v1/recovery/complete` now require `recovery:admin`; `/v1/backups/start`,
`/v1/backups/complete`, `/v1/restore-tests/start`,
`/v1/restore-tests/complete` now require `backups:admin`. Regression tests
(`TestRecoveryMutationsRequireAdminScope`, `TestBackupMutationsRequireAdminScope`)
exercise the real HTTP handlers with a scopeless bearer token (403 on every
mutating route) and an admin-scoped token (200). Residual risk: state is
still in-memory with no durable cross-restart audit trail, and this fix does
not address how `recovery:admin`/`backups:admin` scopes get issued
operationally.

## 8. Exploit chains

CHAIN-001: local/network caller → spoofed identity headers → AUDIT-001 shell
execution → raw output persisted → AUDIT-004 result/restore disclosure. Conditions:
service reachability and writable/ readable container context. Reliability: high
once reachable. Detection: ordinary HTTP logs may reveal requests; no validated
issuer audit trail. Break by central auth plus command capability isolation.

CHAIN-002: reachable caller → AUDIT-002 token issuance → downstream acceptance as
arbitrary tenant/principal → AUDIT-003 control-plane mutation. Reliability and
final privilege depend on unverified downstream token consumption; therefore this
chain is plausible, not confirmed end-to-end.

## 9. Invariant register

| Invariant | Enforcement | Alternate paths | Risk/result |
|---|---|---|---|
| Identity is issuer-verified | header equality/body fields | every direct service route | contradicted; AUDIT-001..003 |
| Results belong to requesting principal/tenant | none in proxy result handlers | list, ID, restore | unenforced; AUDIT-004 |
| Operation IDs are unique and owner-bound | caller ID accepted; map overwrite | dry-run and execute | contradicted; AUDIT-004 |
| Extension signature binds content/publisher | non-empty string only | install aliases/dry-run | unenforced; AUDIT-003 |
| RLS tenant context is correct | SQL policies, caller-set context | workers/direct services | partially enforced; live DB gap |
| Default credentials are never deployed | placeholder default | environment omission | contradicted; AUDIT-005 |

## 10. Assumption register

Internal-only services: convention only. Trusted gateway headers: unenforced in
this repository. Webhook/queue authenticity and exactly-once execution: not
demonstrated. Caller-supplied tenant/principal: contradicted by attacker model.
RLS correctness after migration: partially enforced, live database not verified.

## 11. Missing-test matrix

Missing endpoint auth rejection; mismatched/forged headers; cross-tenant result
reads; operation-ID collision/ownership; shell metacharacters and working-dir
policy; issuer authentication and token expiry bounds; cryptographic signature
binding; duplicate/replayed completion; concurrent transitions; persist-failure
rollback; live PostgreSQL/RLS; Docker topology; and multi-service integration.

## 12. Fix log

AUDIT-001..005: implemented in `packages/service-auth`, all HTTP service
boundaries, execution-proxy ownership checks, security-gate issuance,
extension-manager signature verification, `.env.example`, and Compose. Existing
worker test changes were preserved. `go test ./...`, `go vet ./...`, targeted
Python tests, and Compose validation with explicit secrets passed.

## 13. Suspicious areas requiring further validation

Live auth propagation from gateway to all services; production network exposure;
database role/RLS behavior; queue retry/deduplication; extension runtime loading;
and backup/recovery durable authorization remain further validation areas.

## 14. Defense-in-depth improvements

Centralize authenticated service identity; derive tenant/principal from verified
claims, never caller JSON; authorize at effect boundaries; use private networks;
remove shell execution or use an allowlisted argv runner; use random server-owned
IDs and owner-bound result handles; cryptographically verify manifests; require
non-default secrets; and add integration/race/replay/negative tests.

## 15. Three completeness-pass results

See `audit-results/three-completeness-passes.md`. Pass 1 found result disclosure;
pass 2 found operation-ID/state abuse; pass 3 found the default credential and
unified-authentication gap.

## 16. Multi-agent review record

The master, control-flow, data-flow, authorization, authentication, workflow,
business-logic, concurrency, error, configuration, persistence, integration,
dependency, test-gap, exploit-chain, dead-corner, and blind-spot skills each
have a persistent artifact. They ran sequentially under `coordinator-sequential`
because no delegation tool was available. High candidates were independently
cross-reviewed; the security-gate race suspicion was rejected.

## 17. Blind-spot auditor results

The final blind-spot pass reviewed supposedly internal/operational/clean areas,
adapters, compatibility, migrations, workers, and deployment. It found the
default credential and operation-ID/result ownership weaknesses that were not
fully represented in the inherited report. No additional confirmed Critical
issue was established.
