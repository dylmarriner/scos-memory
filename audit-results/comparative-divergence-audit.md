# Comparative audit: scos-memory-fabric vs scos-memory

Date: 2026-08-27. Method: static analysis of both working trees + git history.
Full report (artifact): https://claude.ai/code/artifact/f9781846-198f-4e7e-9b46-e205210cc455

## Lineage

Common ancestor `d40ac4f` (2026-08-04). Since the fork:
- fabric: +28 commits (persistence, mcp-gateway, graphify, embeddings, TLS, CI)
- scos-memory: +1 commit (`32c0460`) — hostile audit completed, 7 findings fixed

## Findings

| ID | Sev | Scope | Summary |
|----|-----|-------|---------|
| F-01 | Critical | fabric | Tenant identity is client-asserted: shared bearer token + caller-supplied `X-SCOS-Tenant-ID` preserved (mcp-gateway main.go:232) and forwarded (:781); downstream `resolveIdentity` trusts headers when no claims. Confirmed in this repo's own untracked `hostile-exploit-chain-auditor.md`, never fixed. |
| F-02 | High | fabric | No authorization layer at all — zero scope/role checks. scos-memory has `serviceauth.RequireScope` on 18/18 services. |
| F-03 | High | fabric | recovery-orchestrator + backup-manager mutating routes have blanket auth only. Verbatim AUDIT-007, fixed in scos-memory, never ported. |
| F-04 | High | fabric (uncommitted) | `resolveWorkDir` containment is lexical (`filepath.Clean`) with no `EvalSymlinks`; `os.Stat` follows symlinks. Also fails open when `GRAPHIFY_ALLOWED_ROOT` unset. |
| F-05 | High | fabric (uncommitted) | Compose default mounts `/media/kubuntux/DEVELOPMENT1` (~90 projects incl. Securestore, founder-vault) into a network-reachable service. |
| F-06 | Medium | fabric (uncommitted) | User positionals (`q`/`from`/`to`/`node`) reach `graphify` argv with no `--` terminator → argument injection. |
| F-07 | Medium | fabric (uncommitted) | `cacheKey` path-flattening collides (`/a/b` vs `/a_b`); `ensureCachedGraph` has no lock and accepts partial `graph.json` forever. |
| F-08 | High | both | RLS inert as deployed: no `FORCE ROW LEVEL SECURITY` anywhere; services connect as the table-owning role, which Postgres exempts. fabric's `rls_test.go` proves policies work only under a fixture-created `NOBYPASSRLS` role that no migration or compose file creates. |
| F-09 | Medium | both | Tables without RLS: 6 in fabric, 11 in scos-memory. Both include `namespace_memberships` — the ACL table other policies subquery. |
| F-10 | Medium | both | Claims tokens have no `jti`, audience, issuer check, or revocation. HS256 header is correctly pinned. |
| F-11 | High | scos-memory | 14 of 18 services keep authoritative state in in-memory maps; restart loses all governance state. Contradicts documented Postgres authority. |
| F-12 | Medium | scos-memory | `.env.example` ships `sslmode=disable` + placeholder passwords; fabric requires `sslmode=require` and fails closed on empty secrets. |
| F-13 | Medium | scos-memory | No CI, no mcp-gateway, no agent integrations — not reachable by any client. |
| F-14 | High | fabric | Audit ran 2026-08-23 producing 15 specialist reports and no synthesis: missing final-report, fix-log, cross-review, coverage-ledger, blind-spot pass. Reports untracked in git. F-01 was found and left in place. |
| F-15 | Medium | fabric | `scripts/scos-migrate` applies a file then records the version in a separate statement (non-atomic). Two policy files share number `000008`. |

## Merge-back

scos-memory → fabric: `packages/service-auth` + all `RequireScope` call sites; AUDIT-006 tenant-from-verified-identity pattern; AUDIT-007 scopes; completed audit artifact set.

fabric → scos-memory: migrations 000007–000020 + store layer; `database/integration/rls_test.go`; CI; fail-closed env config; mcp-gateway, embeddings worker, agent integrations.

## Recommended order

1. mcp-gateway: derive tenant/principal solely from validated claims, overwrite inbound `X-SCOS-*`; issue per-tenant tokens (F-01).
2. Narrow graphify mount; fail closed on unset allowed root; `EvalSymlinks` before prefix check (F-04, F-05).
3. Create `NOSUPERUSER NOBYPASSRLS` app role, add `FORCE ROW LEVEL SECURITY`, close uncovered tables, put `rls_test.go` in CI (F-08, F-09).
4. Port the scope layer into fabric (F-02, F-03).
5. Finish the audit already run (F-14).
6. Decide whether scos-memory remains a live branch — maintaining both is how F-03 arose.

## Limits

Static only; nothing deployed or exploited live. scos-memory `go build ./...` and `go vet ./...` both exit 0; fabric's full build exceeded a 5-minute budget and was not verified end to end (single-package build succeeded). Tests counted, not executed. F-01/02/03 derived independently, then corroborated against this repo's untracked reports. Not reviewed: GOVERNANCE.md vs implementation, OKF Git store, dashboard, Python worker logic, `packages/tlsserve`.
