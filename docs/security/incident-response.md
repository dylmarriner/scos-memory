# Incident response

This is the runbook for security and availability incidents. For a plain
process crash or storage failure with no suspected compromise, use
[`docs/runbooks/crash-recovery.md`](../runbooks/crash-recovery.md) or
[`docs/runbooks/backup-recovery.md`](../runbooks/backup-recovery.md) instead
— this document is for incidents where trust in the system's data or access
control is in question.

## Severity

- **SEV1** — suspected unauthorized access to durable memory, OKF content, or
  secrets; a governed route reachable without the control-plane token; an
  audit log gap.
- **SEV2** — a service is compromised or crash-looping in a way that could
  mask SEV1 activity; a dependency CVE with a known exploit is present in a
  running deployment.
- **SEV3** — a CI security gate (govulncheck, pip-audit, Trivy, gitleaks)
  fails on a change that hasn't shipped yet.

## Immediate actions (SEV1/SEV2)

1. **Rotate the affected credential(s) first, investigate second.** Rotating
   `SCOS_INTERNAL_SERVICE_TOKEN`, `SCOS_CONTROL_PLANE_TOKEN`,
   `SCOS_SECURITY_ISSUER_SECRET`, or `POSTGRES_PASSWORD` requires a
   coordinated restart of every service reading `.env` — do this even before
   root-causing, since every minute a compromised credential stays live is
   exposure.
2. **Pull the audit trail** from `security-gate`'s `/v1/audit` and
   `/v1/audit/{id}` endpoints for the affected window. This is the
   authoritative record of `allowed`/`denied`/`error` results per principal,
   action, and resource — start here, not from service logs.
3. **Check RLS, don't assume it held.** If cross-tenant data exposure is
   suspected, verify the row-level-security policies in
   `database/policies/` actually cover the table in question — a new table
   added without a corresponding RLS policy is the most likely way this kind
   of incident happens.
4. **Isolate, don't delete.** Stop the affected service (`docker compose stop
   <service>`) rather than removing containers or volumes — recovery and
   post-incident review both need the state as it was found. See the crash
   recovery runbook for how services reconcile state on restart.

## Investigation

- Cross-reference `security-gate` audit records with each service's
  structured logs (`packages/httpmid.NewLogger` emits JSON with `service`,
  `method`, `path`, `status`, `duration_ms` on the services that have adopted
  it; broader rollout is tracked, see the README's Technical Highlights).
- Check whether the incident is a CI-caught issue that reached production
  anyway (i.e., a security gate that was non-blocking at the time — see
  `.github/workflows/ci.yml` for which gates currently `continue-on-error`
  and why).
- For a suspected supply-chain issue, pull the CycloneDX SBOM artifact from
  the CI run nearest the incident window (`.github/workflows/ci.yml`'s
  `security` job) to establish exactly what was running.

## Containment and recovery

1. Patch or roll back the vulnerable component.
2. Re-run `make audit` and the full CI security job before redeploying —
   don't redeploy on a hunch that it's fixed.
3. Restore from backup only via the tested path in
   [`docs/runbooks/backup-recovery.md`](../runbooks/backup-recovery.md); an
   untested restore during an incident is how incidents become disasters.

## Post-incident

- Write down what happened, the timeline, and the root cause in a dated
  entry — this repository doesn't yet have a formal postmortem template;
  until it does, a dated file under `docs/adr/` or an issue with the
  `security` label is the interim home for it.
- If the root cause is a missing control (e.g., a table without RLS, a route
  without the control-plane token check), add it to
  [`docs/security/threat-model.md`](threat-model.md)'s mitigation table so
  the next reviewer sees it was addressed, not just fixed silently.

## Reporting

External reports come in via
[GitHub Security Advisories](https://github.com/dylmarriner/scos-memory/security/advisories/new)
per [`SECURITY.md`](../../SECURITY.md). Do not discuss suspected
vulnerabilities in public issues or PRs before a fix ships.
