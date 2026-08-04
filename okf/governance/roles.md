---
title: Governance Roles
description: Auditable role assignments for SCOS Memory Fabric, binding the logical roles defined in GOVERNANCE.md §34 to named principals.
tags: [governance, roles, audit]
status: active
---

# Governance Roles

GOVERNANCE.md §34 defines eight logical roles. This document is the auditable
assignment record: who currently holds each role, and the audit trail of
changes. One person MAY hold several roles; responsibilities remain distinct
and MUST be exercised distinctly (an assignment does not merge the underlying
authority).

## Current assignments

| Role | Principal | Since | Notes |
|---|---|---|---|
| System owner | Dylan Marriner | 2026-07-26 | Direction, governing policy, architecture and risk acceptance. |
| Memory steward | Dylan Marriner | 2026-07-26 | Taxonomy, promotion, retention, correction and retrieval quality. |
| Security steward | Dylan Marriner | 2026-07-26 | Identity, permissions, threats, secrets and incidents. |
| Knowledge curator | Dylan Marriner | 2026-07-26 | OKF quality, sources, concepts and deprecation. |
| Agent maintainer | Dylan Marriner | 2026-07-26 | Registration, interfaces, permissions and evaluation. |
| Autonomy steward | Dylan Marriner | 2026-07-26 | Routing policy, capability contracts, confidence thresholds, route quality and safe fallback behaviour. |
| Operator | Dylan Marriner | 2026-07-26 | Deployment, monitoring, backups, recovery and capacity. |
| Auditor | Dylan Marriner | 2026-07-26 | Independent review of privileged actions and policy compliance. |

## Reassignment procedure

1. Propose the change with rationale in a PR touching this file.
2. The current System owner (or, if the System owner role itself is being
   reassigned, the outgoing holder) approves the PR.
3. The merge commit hash and date become the new `Since` value.
4. Historical assignments are retained below rather than overwritten in
   place, so the audit trail is never lost to an edit.

## Assignment history

| Role | Principal | From | To | Change reason |
|---|---|---|---|---|
| (all roles) | Dylan Marriner | 2026-07-26 | present | Initial governance baseline; single-holder bootstrap per §34 ("One person MAY initially hold several roles"). |

## Separation-of-duty notes

- The **Auditor** role SHOULD NOT approve its own privileged actions. While
  currently held by the same principal as System owner during bootstrap,
  any audit finding against a System-owner decision MUST be recorded here
  rather than silently reconciled, until a distinct Auditor principal is
  assigned.
- The **Security steward** and **Autonomy steward** hold the two roles most
  likely to expand system authority (permissions, routing, confirmation
  thresholds). Changes attributed to either role SHOULD reference the
  governing ADR in `docs/adr/`.
