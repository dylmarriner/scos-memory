# Threat model

Scope: the governed service mesh (`services/`), shared libraries
(`packages/`), the Compose deployment (`deploy/`), and PostgreSQL. This
complements [`docs/security-configuration.md`](../security-configuration.md)
(what to configure) with *why* those controls exist.

## Assets

1. **Conversational history** — every durably journaled turn, message, and
   tool operation (`services/memory-gateway`). High volume, ranges from
   low to high sensitivity per record.
2. **Promoted long-term memory and OKF content** — curated knowledge that
   has passed review (§9 of `GOVERNANCE.md`). Lower volume, higher trust —
   compromising this poisons every future agent decision that reads it.
3. **Secrets and credentials** — internal bearer tokens, the control-plane
   token, the security issuer secret, and any `SecretReference` entries
   `security-gate` resolves against an external secret manager.
4. **Audit records** (`security-gate`'s audit log) — the record of who did
   what; tampering here breaks the ability to detect everything else.

## Trust boundaries

```text
Untrusted / external agents & tools
        │  (bearer token required)
        ▼
Autonomous Control Plane  ── X-SCOS-Control-Plane-Token required ──▶ Governed services
        │                                                                  │
        ▼                                                                  ▼
security-gate (auth, permission, audit, secret refs)              PostgreSQL (RLS-scoped)
```

Two tokens gate two different boundaries, deliberately:

- **`SCOS_INTERNAL_SERVICE_TOKEN`** — proves a caller is *an* internal
  service. Necessary but not sufficient for governed routes.
- **`SCOS_CONTROL_PLANE_TOKEN`** — proves a caller is *the* autonomous
  control plane specifically. Governed service routes reject direct calls
  that present only the internal bearer token; see
  `docs/security-configuration.md`. This exists so a compromised or
  misbehaving peripheral service can't directly drive a governed action —
  it has to go through the control plane's journaling and scope resolution.

## Key threats and mitigations

| Threat | Mitigation | Where |
|---|---|---|
| A compromised service reads another tenant's data directly from Postgres | Row-level security on every data-plane table, not just application-layer tenant filtering | `database/policies/*_rls.sql` |
| A caller bypasses the control plane to invoke a governed action directly | Control-plane token required on governed routes, rejected even with a valid internal bearer token | `packages/auth`, `docs/security-configuration.md` |
| Secrets committed to the repo or shipped in an image | `.gitignore` excludes `.env`, `*.key`, `*.pem`, `tls-external/`; base Compose has no secret defaults, service startup fails closed without them; CI runs gitleaks on every push | `.gitignore`, `Makefile` `validate` target, `.github/workflows/ci.yml` |
| A dependency or base image ships a known CVE | `govulncheck` (Go), `pip-audit` (Python), and Trivy (container image) run in CI; Dependabot opens PRs for new advisories | `.github/workflows/ci.yml`, `.github/dependabot.yml` |
| An unhandled panic in one request handler takes the whole service down | Panic-recovery middleware returns 500 instead of crashing the process (rolled out to the request-critical services first) | `packages/httpmid.Recover` |
| A crash or forced restart loses in-flight work | Synchronous durable commit for authoritative writes, checkpoint/recovery on startup, graceful shutdown drains in-flight requests before exit | `GOVERNANCE.md` §30, `packages/tlsserve.ServeGracefully` |
| An agent's output is treated as trusted fact without review | Promotion to durable memory is a separate, reviewed step from journaling — an agent cannot make its own output authoritative | `GOVERNANCE.md` §9, consolidation worker review path |
| Disk exhaustion from unbounded journal/telemetry growth | Reserved journal capacity, bounded artifact/index growth, operator alerting (§30) | `GOVERNANCE.md` §30 |

## Explicitly out of scope

- Physical security of the deployment host.
- Compromise of the operator's own machine/credentials used to configure
  `.env` or the secret manager.
- Attacks requiring an already-valid `SCOS_CONTROL_PLANE_TOKEN` performing
  actions that token is documented to authorize (see `SECURITY.md`).

## Reporting a new threat

See [`SECURITY.md`](../../SECURITY.md) — use GitHub Security Advisories, not a
public issue, for anything not already covered by the mitigations above.
