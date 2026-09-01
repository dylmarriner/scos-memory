Skill: hostile-repository-auditor (implementation-assurance verification pass)
Agent: fork of the implementation-assurance coordinator session, 2026-08-29
Scope: services/security-gate only (agent-registry, autonomous-control-plane, execution-proxy handled by sibling passes)

Files/components inspected:
- services/security-gate/cmd/server/main.go (full, 916 lines pre-fix)
- services/security-gate/cmd/server/store.go (full, 364 lines)
- services/security-gate/cmd/server/main_test.go (full, pre- and post-fix)
- database/migrations/000013_security_gate_persistence.sql (security_tokens, secret_references schema)
- database/migrations/000001_initial.sql / 000002_governance_tables.sql (audit_entries schema + RLS)
- Prior artifacts re-verified against current code: hostile-authentication-identity-auditor.md, hostile-error-path-auditor.md

Entry points inspected:
- POST /v1/auth/issue, /v1/auth/validate, /v1/auth/revoke
- POST /v1/permissions/check, /v1/permissions/classify
- GET /v1/audit, GET /v1/audit/{id}, POST /v1/audit/record
- GET/POST /v1/secrets, GET /v1/secrets/{id}, POST /v1/secrets/{id}/resolve
- GET /health/live, /health/ready
All routes are registered on one mux wrapped entirely in `auth.AuthenticatedHandler` (main.go:233), i.e. every caller must already hold a valid fleet-wide claims bearer token before reaching any handler, including `/v1/auth/issue`. This resolves the apparent "bootstrapping" question: `/v1/auth/issue` is not the fleet's root of trust — it is double-gated (fleet claims token via the outer middleware, AND `X-SCOS-Issuer-Secret` via `validIssuerSecret`, main.go:264/318-322) to mint security-gate's own, separate token type. No `auth.RequireScope` is used anywhere in this service; confirmed by grep. The RequireScope mux-test-gap pattern from recovery-orchestrator/backup-manager does not apply here — there is no scope gating to have a gap in.

Important callers/callees:
- handleAuthIssue -> validIssuerSecret -> newTokenID (CSPRNG public id + HMAC signature) -> insertToken (store.go:101)
- handleAuthValidate -> validToken (full HMAC check) -> lookupToken (store.go:125, unscoped by tenant — justified, see below)
- handleAuthRevoke -> [FIXED] validToken -> lookupToken -> revokeToken (store.go:158)
- handlePermissionCheck/handleClassifyData -> authenticatedToken (main.go:360, Authorization header, full validToken + DB lookup) -> evaluatePermission
- handleSecrets/handleSecretByID/handleSecretResolve -> resolveIdentity (store.go:30, claims-first, `X-SCOS-*` header fallback for service-to-service calls) -> setPrincipalContext (store.go:90, sets `scos.tenant_id`/`scos.principal_id` for RLS) -> secret_references / audit_entries queries
- resolveSecretValue -> securestoreClient.getSecret -> Securestore REST API (external trust boundary)

Workflows traced:
1. Token lifecycle: issue (double-gated) -> validate (full HMAC + DB) -> revoke. Revoke was the only leg that did not re-derive the HMAC signature before acting (see Findings).
2. Permission check: identity taken from the authenticated security-gate token (not the request body) at main.go:454, with a body-supplied principal/tenant only allowed if it agrees with the token (main.go:459-466) — correct precedence, caller cannot spoof identity via body.
3. Secret resolution: getSecretReference is RLS/tenant-scoped by claims-derived identity (never by caller-supplied body), then resolveSecretValue fails closed on every error branch: missing config (503), not found (404), upstream transport error (502, audited as "error"), non-2xx (502), `approval_required` or empty value (503, audited as "denied"). Verified in code and by the existing tests `TestResolveSecretValueFailsClosedWhenSecurestoreUnreachable/OnApprovalRequired/OnUpstreamErrorStatus`. The hostile-error-path-auditor.md claim is confirmed true against current code.
4. securestoreClient.getSecret (main.go:884-909): single HTTP round trip, no retry, no cache — confirmed against the hostile-error-path-auditor.md claim ("getSecret ... never retries"); this is documented as intentional (comment at main.go:882-883: freshness over resilience so a revoked/rotated credential is reflected immediately), not a defect.

Trust boundaries:
- Fleet claims-token boundary (packages/auth.AuthenticatedHandler) in front of the entire mux.
- security-gate's own bearer-token boundary (public-id + HMAC signature, `security_tokens` table) in front of /v1/permissions/*.
- security-gate -> Securestore REST API (external secret manager), authenticated with a static API key from `SCOS_SECURESTORE_AGENT_API_KEY`.
- RLS boundary on `audit_entries`/`secret_references` via `set_config('scos.tenant_id', ...)`; `security_tokens` intentionally has no RLS (store.go:119-124 comment: primary key is a globally-unique 32-byte CSPRNG value, so a single-row keyed lookup cannot leak cross-tenant).

Attacker-controlled inputs:
- `POST /v1/auth/revoke` body `{"token": "..."}` — prior to the fix, only the public-id half was used (`tokenPublicPart`, which safely no-ops if there's no "."), so any string containing (or equal to) a valid public id revoked that token, with no proof of possession and no tenant check.
- `POST /v1/secrets` body (id/name/provider/ref/scope) — validated against `validSecretProviders` allowlist before persistence.
- `POST /v1/permissions/check` body `Principal`/`Tenant`/`DataClass` — only accepted if it agrees with the authenticated token's identity, otherwise 403.

Invariants tested:
- Revocation requires proof of possession of the real bearer token (public id + correct HMAC signature), not merely knowledge of the public id — added `TestAuthRevokeRequiresProofOfPossession`.
- Secret resolution fails closed on every upstream error branch (pre-existing tests, re-verified).
- Permission checks cannot have their principal/tenant spoofed via request body (pre-existing test `TestPermissionCheckRejectsForgedToken`, re-verified).

Findings:
1. [Confirmed — fixed this session] `handleAuthRevoke` (main.go, pre-fix line ~412-439) never called `s.validToken(req.Token)` before revoking. It called `lookupToken(ctx, db, tokenPublicPart(req.Token))` directly; `tokenPublicPart` returns its input unchanged when there is no "." separator, so a caller who supplied only a token's *public id* (no HMAC signature at all) could revoke it. Any authenticated fleet-wide bearer-token holder (this service's entire mux is gated only by the outer `auth.AuthenticatedHandler`, not by scope) who learned another principal's security-gate token's public id — e.g. from that tenant's own `GET /v1/audit` listing, which records `AuditRecord.Resource = tokenPublicPart(tokenID)` for "issued"/"revoked" actions (main.go:306, 434) — could revoke that principal's live token with zero proof of possession, a same-tenant griefing/DoS primitive. `revokeToken` (store.go:158-168) additionally has no tenant filter in its `WHERE` clause, so the only thing preventing cross-tenant revocation was obscurity of the public id, not any check.
   - Reproduction: mutation-tested by removing the added `validToken` check and re-running `TestAuthRevokeRequiresProofOfPossession` — it failed with a nil-pointer panic inside `lookupToken`'s `db.QueryRowContext` (test server has no `s.db` configured, matching how every other handler in this file is exercised), proving the code path was reached with only a bare public id and no signature. Restored the fix; test passes.
   - Fix: `services/security-gate/cmd/server/main.go` — added `if !s.validToken(req.Token) { http.Error(w, "invalid token", http.StatusUnauthorized); return }` immediately after decoding the request body in `handleAuthRevoke`, mirroring the check `handleAuthValidate` already performs. This forces proof of possession of the full bearer token before any revocation can occur, which subsumes the missing tenant check (a caller who can produce a valid signature for a given public id must already hold that exact token).
   - Test: `TestAuthRevokeRequiresProofOfPossession` (services/security-gate/cmd/server/main_test.go) — asserts 401 for (a) the bare public id and (b) the public id with a forged signature suffix.

2. [Non-issue, documented] `security_tokens` has no RLS and `lookupToken` intentionally runs without setting `scos.tenant_id`/`scos.principal_id`. This is safe as designed: the table's primary key is a 32-byte CSPRNG-derived public id (effectively unguessable), and lookup is always by that exact key, never by a filtered scan. Re-verified the comment against the schema (000013_security_gate_persistence.sql) and confirmed no other query path scans `security_tokens` without a key.

3. [Non-issue, documented] "Only security-gate can mint claims tokens accepted fleet-wide" was already found false by hostile-authentication-identity-auditor.md (AUTH-2: other services, e.g. cloud-control-plane, mint fleet claims tokens locally via `auth.SignClaims`). That is an architectural fact about the fleet-wide claims system, not a defect in security-gate itself — security-gate's own token type (`security_tokens`) is a separate, self-contained system only its own `/v1/permissions/*` handlers consult, and that separation is unaffected by AUTH-2. No action taken; flagging as still true and out of this service's remediation scope.

4. [Hardening opportunity, not fixed] None of security-gate's own mutating endpoints (`/v1/auth/issue`, `/v1/secrets` POST, `/v1/audit/record`) are scope-gated beyond the blanket fleet bearer-token check (plus, for `/v1/auth/issue`, the separate issuer-secret check). Any principal holding a valid fleet claims token — regardless of role — can create secret references, write arbitrary audit records via `POST /v1/audit/record` (identity-checked, but any authenticated principal can write an "allowed" audit entry for themselves attesting anything), or (with the issuer secret) mint security-gate tokens for any UUID principal/tenant pair. This mirrors the AUDIT-007 class of gap already remediated with `auth.RequireScope` in recovery-orchestrator/backup-manager, but is architecturally different here (there is no privileged/non-privileged split among security-gate's own callers documented anywhere), so introducing a new scope name would be a product decision, not a bug fix — not implemented in this session per the "do not redesign the token-type architecture" scope constraint.

Suspicions requiring validation:
- Whether any deployed caller relies on being able to revoke a token it does not hold (e.g. an admin/ops tool that only ever has the public id, not the full token) — the fix would break that caller if it exists. Grepped `services/` and `deploy/` for callers of `/v1/auth/revoke`; found none outside this service's own tests, so no such caller is currently in the repo, but this cannot rule out an external operator tool.
- Whether `POST /v1/audit/record`'s self-attested audit entries (any authenticated principal can write their own "allowed"/"denied" audit record with an arbitrary `Reason`) are ever treated as an authoritative decision log by a downstream consumer, which would make finding 4 more than hardening. Not traced — downstream consumers of `audit_entries` were out of this service's scope.

Tests/commands executed:
- `go build ./...` (repo root) — clean, no errors.
- `go test ./services/security-gate/...` — all pass, including the new `TestAuthRevokeRequiresProofOfPossession`.
- `go test ./...` (repo root, full suite) — all packages pass or report "no test files" (packages/llmclient, pre-existing/unrelated).
- Mutation test: temporarily removed the `validToken` guard from `handleAuthRevoke`, re-ran the new test (failed with a panic, proving the check is load-bearing), then restored the fix and re-confirmed `go build ./...` / `go test ./services/security-gate/...` clean.
- Note: during this session a `cp` restore step from a shared scratchpad backup transiently clobbered `services/security-gate/cmd/server/main.go` with unrelated `code-intelligence` content (a filename collision in a session-shared scratch directory, not a git operation). Caught immediately via `git diff --stat` before any test run against the corrupted state, restored with `git checkout -- services/security-gate/cmd/server/main.go`, and the fix was reapplied cleanly. `services/security-gate/cmd/server/main_test.go` was never affected. Flagging for transparency since the audit trail should not hide a self-inflicted near-miss, even though it never reached a test run or commit.

Coverage gaps:
- No test previously existed for `handleAuthRevoke` at all (confirmed by grep of main_test.go before this session's edit) — the endpoint was completely untested, which is how the missing signature check went unnoticed.
- No test exercises `/v1/audit/record`'s self-attestation behavior (finding 4) one way or the other.

Areas explicitly not reviewed:
- Downstream consumers of `audit_entries` written by `POST /v1/audit/record` (out of security-gate's own scope).
- The fleet-wide claims-token issuance system (packages/auth, cloud-control-plane) — only referenced for context on AUTH-2, not re-audited here.
- Load/concurrency behavior of `security_tokens` under high issue/revoke throughput.

Recommended follow-up:
- Product decision needed on whether `/v1/auth/issue`, `/v1/secrets` POST, and `/v1/audit/record` warrant `auth.RequireScope` gating (finding 4) — flagging rather than implementing, since there is no existing scope taxonomy for security-gate's own privileged operations to reuse.
- Consider whether `POST /v1/audit/record`'s self-attested entries should be marked distinctly (e.g. a `source: self-reported` field) from entries security-gate itself generates as a direct result of a checked decision, so downstream consumers can tell the two apart.
