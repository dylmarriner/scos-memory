export const meta = {
  name: 'hostile-repo-audit-scos-fabric',
  description: 'Full 16-specialist hostile audit of scos-memory-fabric with cross-review, 3 completeness passes, blind-spot pass',
  phases: [
    { title: 'Specialists' },
    { title: 'CrossReview' },
    { title: 'CompletenessPasses' },
    { title: 'BlindSpot' },
  ],
}

const REPO = '/media/kubuntux/DEVELOPMENT1/scos-memory-fabric'

const FINDING_SCHEMA = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          id: { type: 'string' },
          title: { type: 'string' },
          severity: { type: 'string', enum: ['Critical', 'High', 'Medium', 'Low', 'Info'] },
          confidence: { type: 'string', enum: ['Confirmed', 'High-confidence', 'Plausible', 'Hardening opportunity', 'Non-issue'] },
          file: { type: 'string' },
          summary: { type: 'string' },
        },
        required: ['title', 'severity', 'confidence', 'summary'],
      },
    },
    coverage_gaps: { type: 'array', items: { type: 'string' } },
    artifact_written: { type: 'boolean' },
  },
  required: ['findings', 'artifact_written'],
}

const SPECIALISTS = [
  { name: 'hostile-control-flow-auditor', hint: 'Trace mcp-gateway, tool-governance, execution-proxy, autonomous-control-plane request handling from entry to terminal effects (subprocess exec, DB writes, network calls). Focus on services/execution-proxy and services/tool-governance callers/callees end to end.' },
  { name: 'hostile-data-flow-auditor', hint: 'Trace attacker-controlled input (headers, request bodies, env, webhook/extension payloads) through services/extension-manager, services/graphify-provider, services/code-intelligence, services/context-optimisation to storage/exec/network sinks. Include second-order (stored-then-later-used) paths.' },
  { name: 'hostile-authorization-auditor', hint: 'Re-verify and extend the known cross-tenant identity-spoofing pattern (shared SCOS_INTERNAL_SERVICE_TOKEN + client-supplied X-SCOS-Tenant-ID/X-SCOS-Principal-ID headers trusted without cross-check) across ALL services, not just memory-intelligence/mcp-gateway/memory-gateway already found. Check services/retrieval-orchestrator, services/agent-registry, services/graphify-provider, services/code-intelligence, services/context-optimisation, services/observability, services/backup-manager, services/recovery-orchestrator for the same resolveIdentity/header-trust pattern.' },
  { name: 'hostile-authentication-identity-auditor', hint: 'Audit packages/auth token issuance/verification/revocation lifecycle, services/security-gate token issue/validate endpoints, service-to-service identity propagation across all ~20 services in services/. Check for token revocation support (is there any way to invalidate SCOS_INTERNAL_SERVICE_TOKEN short of redeploy?).' },
  { name: 'hostile-workflow-state-machine-auditor', hint: 'Reconstruct state machines for: services/tool-governance operation lifecycle (prepare/execute/complete), services/recovery-orchestrator recovery flow, services/backup-manager backup/restore flow, services/okf-synchroniser sync/proposal flow. Look for forbidden-transition bypass, stale-state, and duplicate-execution bugs.' },
  { name: 'hostile-business-logic-abuse-auditor', hint: 'Attack product invariants in services/okf-synchroniser (proposal approval/promotion thresholds SCOS_OKF_PROMOTION_AUTHORITY_THRESHOLD/CONFIDENCE_THRESHOLD), services/tool-governance rule approval, services/extension-manager install/approval flow using only legitimate valid-input sequences (e.g. can a low-authority proposal be promoted by resubmission, race, or partial-approval abuse?).' },
  { name: 'hostile-concurrency-auditor', hint: 'Look for races/TOCTOU in services/execution-proxy operation_id issuance-then-execution, services/tool-governance approval-then-execution, workers/consolidation and workers/index-maintenance job claiming, and any check-then-act pattern around database/policies RLS context (set_config then query in a pooled-connection service).' },
  { name: 'hostile-error-path-auditor', hint: 'Treat every failure branch in services/backup-manager, services/recovery-orchestrator, services/security-gate secret resolution, and database/migrations rollback as a separate workflow. Look for state left behind after a failed operation, swallowed errors, and insecure fallback (fail-open) behavior anywhere auth/tenant checks are involved.' },
  { name: 'hostile-configuration-auditor', hint: 'Audit deploy/compose/docker-compose.yml, deploy/native/env/*, and every services/*/cmd/server/main.go for env-var defaults that are unsafe if unset in production (fail-open vs fail-closed), dev/prod config divergence, and any debug/insecure flag reachable via env var.' },
  { name: 'hostile-persistence-auditor', hint: 'Audit database/migrations and database/policies for every table: RLS presence, tenant_id enforcement, soft-delete/restore correctness, cascade behavior, and any table missing RLS that holds tenant-scoped data. Cross-check against services that write to it.' },
  { name: 'hostile-integration-auditor', hint: 'Audit services/extension-manager hook dispatch (signature verification, replay protection, retry/duplicate delivery), services/hermes-provider and services/graphify-provider external-facing integration points, and any webhook-like callback path for trust assumptions about the caller.' },
  { name: 'hostile-dependency-supply-chain-auditor', hint: 'Focus specifically on services/extension-manager extension install (Ed25519 verification path vs the dead sha256 VerifySignature helper in packages/extension-contracts) and on go.mod/package.json version currency and any build-time script execution (Makefile, scripts/) that could execute untrusted code during CI or install.' },
  { name: 'hostile-test-gap-auditor', hint: 'Read tests/ (failure-injection, stress, test_audit_governance_runtime.py) and any *_test.go under services/ and packages/. Identify which of the following have NO negative/authorization/tenant-isolation test: cross-tenant access attempts, expired/malformed token handling, execution-proxy destructive-command handling, RLS bypass attempts.' },
  { name: 'hostile-dead-corner-auditor', hint: 'Search scripts/, hooks/pre-turn, adapters/cli-wrapper, adapters/hermes, adapters/model-proxy, integrations/ (agent-plugins, claude, codex, hermes, kilo, openclaw, opencode, qwen), plugins/example-plugin, and deploy/scripts for forgotten/legacy/debug/admin code paths that bypass normal auth or execute commands.' },
  { name: 'hostile-exploit-chain-auditor', hint: 'Given the KNOWN confirmed findings: (A) shared-token + spoofable tenant/principal headers grants cross-tenant data access on mcp-gateway and memory-intelligence, and (B) services/execution-proxy executes agent-supplied shell commands via /bin/sh -c with only cosmetic risk classification (no real block) — construct concrete end-to-end exploit chains combining these with anything else discovered. E.g.: does identity-spoofing (A) let an attacker reach execution-proxy (B) as a different tenant/principal to attribute malicious command execution to a victim, or escalate from read access to code execution?' },
]

phase('Specialists')
const specialistResults = await parallel(SPECIALISTS.map(s => () =>
  agent(
    `You are the ${s.name} specialist auditor for a hostile security/correctness audit of the repository at ${REPO}.\n\n` +
    `First, Read ${REPO}/skills/${s.name}/SKILL.md in full and follow it exactly as your operating procedure.\n` +
    `Then Read ${REPO}/skills/hostile-repository-auditor/references/artifact-template.md for the required artifact format.\n\n` +
    `Scope hint for this repository (use as a starting point, not a limit — follow the skill's own search strategy too): ${s.hint}\n\n` +
    `Do real investigation: use Read/Grep/Glob/Bash to inspect actual source files, trace actual code paths, and cite exact file:line locations. Do not speculate without evidence.\n\n` +
    `Known context already established elsewhere in this audit (treat as background, verify independently if it falls in your scope, do not just repeat it):\n` +
    `- Services communicate over an internal shared bearer token (SCOS_INTERNAL_SERVICE_TOKEN) plus optional per-request JWT claims (packages/auth/middleware.go).\n` +
    `- mcp-gateway (services/mcp-gateway) is the externally-reachable MCP endpoint (port 8098) used by external Claude Code/Desktop clients per docs/agent-connection-guide.md.\n` +
    `- execution-proxy (services/execution-proxy) executes shell commands on behalf of agents; tool-governance (services/tool-governance) is meant to gate what gets executed.\n` +
    `- Database uses Postgres RLS scoped by tenant_id via set_config('scos.tenant_id', ...) per request (database/policies/).\n\n` +
    `Write your full artifact to ${REPO}/audit-results/${s.name}.md using the Write tool, following the template exactly (Skill/Agent/Scope/Files inspected/Entry points/Callers-callees/Workflows traced/Trust boundaries/Attacker-controlled inputs/Invariants tested/Findings/Suspicions/Tests-commands executed/Coverage gaps/Areas not reviewed/Recommended follow-up). Set Agent to "${s.name}-workflow-agent".\n\n` +
    `Then return a JSON summary per the required schema: every finding you have at least Plausible confidence in (Confirmed/High-confidence/Plausible/Hardening opportunity/Non-issue), with severity, exact file path if applicable, and a one-to-two sentence summary. Also list coverage_gaps (things you did not have time/access to review). Set artifact_written true only if you actually wrote the file.`,
    { label: s.name, phase: 'Specialists', schema: FINDING_SCHEMA }
  ).then(r => ({ specialist: s.name, result: r })).catch(e => ({ specialist: s.name, result: null, error: String(e) }))
))

log(`Specialists complete: ${specialistResults.filter(r => r.result).length}/${SPECIALISTS.length} returned results`)

const allFindings = []
for (const r of specialistResults) {
  if (r.result && Array.isArray(r.result.findings)) {
    for (const f of r.result.findings) {
      allFindings.push({ ...f, specialist: r.specialist })
    }
  }
}
const criticalHigh = allFindings.filter(f => f.severity === 'Critical' || f.severity === 'High')
log(`Total findings collected: ${allFindings.length}, Critical/High: ${criticalHigh.length}`)

phase('CrossReview')
const REVIEWERS = ['hostile-exploit-chain-auditor', 'hostile-blind-spot-auditor', 'hostile-persistence-auditor', 'hostile-authorization-auditor', 'hostile-control-flow-auditor']
const CROSS_REVIEW_SCHEMA = {
  type: 'object',
  properties: {
    finding_title: { type: 'string' },
    verdict: { type: 'string', enum: ['confirmed', 'disproven', 'downgraded', 'insufficient-evidence'] },
    reasoning: { type: 'string' },
    revised_severity: { type: 'string' },
  },
  required: ['finding_title', 'verdict', 'reasoning'],
}
const crossReviews = criticalHigh.length ? await parallel(criticalHigh.map((f, i) => () => {
  const reviewer = REVIEWERS[i % REVIEWERS.length]
  return agent(
    `You are cross-reviewing a Critical/High security finding from another specialist auditor, as the ${reviewer} for repository ${REPO}. Your job is to try to DISPROVE it.\n\n` +
    `Finding under review (from specialist "${f.specialist}"):\nTitle: ${f.title}\nSeverity: ${f.severity}\nConfidence: ${f.confidence}\nFile: ${f.file || 'n/a'}\nSummary: ${f.summary}\n\n` +
    `Read the actual code at the cited location (and its callers/callees/middleware) using Read/Grep. Attempt to falsify: confirm reachability, confirm attacker preconditions actually hold, search for a mitigation on this path or an alternate path, verify the impact is real, and verify severity is not inflated. If you cannot disprove it after genuine effort, mark it confirmed.\n\n` +
    `Return your verdict as structured output.`,
    { label: `review:${f.title.slice(0,40)}`, phase: 'CrossReview', schema: CROSS_REVIEW_SCHEMA }
  ).then(r => ({ finding: f, review: r })).catch(e => ({ finding: f, review: null, error: String(e) }))
})) : []

phase('CompletenessPasses')
const PASS_SCHEMA = {
  type: 'object',
  properties: {
    new_findings: { type: 'array', items: { type: 'object', properties: { title: { type: 'string' }, severity: { type: 'string' }, file: { type: 'string' }, summary: { type: 'string' } }, required: ['title','summary'] } },
    what_this_pass_found_that_others_missed: { type: 'string' },
  },
  required: ['new_findings', 'what_this_pass_found_that_others_missed'],
}
const passResults = await parallel([
  () => agent(
    `Independent completeness pass ONE for repository ${REPO}: "What can an attacker influence, and where can it ultimately reach?" Rebuild input-to-sink analysis from scratch WITHOUT relying on prior audit conclusions — start fresh from every attacker-controlled input source (HTTP requests, headers, webhooks, env vars, files, queue/worker payloads, extension manifests) across the whole services/ tree and trace to sinks (exec, DB, filesystem, network, secrets). Use Read/Grep/Glob. Report only genuinely new findings not likely already found by a services-by-services specialist review, and explicitly state what this pass found that a component-scoped review would likely miss.`,
    { label: 'completeness-pass-1-attacker-inputs', phase: 'CompletenessPasses', schema: PASS_SCHEMA }
  ).catch(e => ({ new_findings: [], what_this_pass_found_that_others_missed: 'agent error: ' + String(e) })),
  () => agent(
    `Independent completeness pass TWO for repository ${REPO}: state-machine and business-logic pass. Ask: what valuable states exist (memory items, operations, proposals, backups, extensions)? What transitions exist and which should be impossible? What happens if an operation runs twice, concurrently, out of order, or after cancellation/deletion/partial failure? What valid sequence of legitimate calls produces an invalid business outcome? Focus on services/tool-governance, services/okf-synchroniser, services/backup-manager, services/recovery-orchestrator, services/extension-manager. Report only genuinely new findings, and state what this pass found that a component-scoped specialist pass would likely miss.`,
    { label: 'completeness-pass-2-state-business-logic', phase: 'CompletenessPasses', schema: PASS_SCHEMA }
  ).catch(e => ({ new_findings: [], what_this_pass_found_that_others_missed: 'agent error: ' + String(e) })),
  () => agent(
    `Independent completeness pass THREE for repository ${REPO}: darkest-corners pass. Concentrate ONLY on: scripts/ (all .py files), hooks/pre-turn, deploy/scripts, deploy/native install scripts, database/integration test helpers, benchmarks/harness.py, .learnings/, indexes/, workers/policy-evaluation and workers/index-maintenance (the two with no dependency manifest), and any file with "legacy", "deprecated", "compat", "migrate", "fallback", "debug", or "admin" in its name or path anywhere in the repo. Report only genuinely new findings, and state what this pass found that mainstream specialist review would likely skip as boring/operational.`,
    { label: 'completeness-pass-3-dark-corners', phase: 'CompletenessPasses', schema: PASS_SCHEMA }
  ).catch(e => ({ new_findings: [], what_this_pass_found_that_others_missed: 'agent error: ' + String(e) })),
])

phase('BlindSpot')
const clean_areas_summary = specialistResults
  .filter(r => r.result)
  .map(r => `${r.specialist}: ${r.result.findings.filter(f => f.confidence === 'Non-issue' || f.severity === 'Info').map(f => f.title).join('; ') || '(no explicit clean items listed)'}`)
  .join('\n')
const blindSpot = await agent(
  `You are the hostile-blind-spot-auditor for repository ${REPO}. Read ${REPO}/skills/hostile-blind-spot-auditor/SKILL.md and follow it exactly.\n\n` +
  `Your explicit mission: prove the rest of this audit missed something. Do NOT re-review what's already been covered in depth (auth/tenant header spoofing, execution-proxy shell exec risk classification — these are already found).\n\n` +
  `Areas other specialists explicitly called clean or low-priority (investigate these FIRST, do not trust the label):\n${clean_areas_summary.slice(0, 4000)}\n\n` +
  `Also specifically check areas not assigned to any specialist above: services/compatibility-gateway, services/agent-registry, services/observability, services/readiness, services/transparency-projection dashboard edit-passphrase flow, packages/httpmid, packages/tlsserve, packages/service-state, packages/routing-contracts, packages/context-contracts, packages/capability-registry, packages/language-adapters, api/contract_coverage_test.go.\n\n` +
  `Write your artifact to ${REPO}/audit-results/hostile-blind-spot-auditor.md per ${REPO}/skills/hostile-repository-auditor/references/artifact-template.md. Return a JSON summary of findings (schema) plus coverage_gaps.`,
  { label: 'blind-spot-final-pass', phase: 'BlindSpot', schema: FINDING_SCHEMA }
).catch(e => ({ findings: [], coverage_gaps: ['blind spot agent error: ' + String(e)], artifact_written: false }))

return { specialistResults, allFindings, criticalHigh, crossReviews, passResults, blindSpot }
