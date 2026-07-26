# Governance roles (§34)

SCOS defines eight logical roles with distinct responsibilities. One person MAY hold
several roles, but the responsibilities remain separate and auditable.

| Role | Responsibilities |
|---|---|
| **System owner** | Direction, governing policy, architecture, risk acceptance |
| **Memory steward** | Taxonomy, promotion, retention, correction, retrieval quality |
| **Security steward** | Identity, permissions, threats, secrets, incident response |
| **Knowledge curator** | OKF quality, sources, concepts, deprecation |
| **Agent maintainer** | Registration, interfaces, permissions, evaluation |
| **Autonomy steward** | Routing policy, capability contracts, confidence thresholds, route quality, safe fallback |
| **Operator** | Deployment, monitoring, backups, recovery, capacity |
| **Auditor** | Independent review of privileged actions, policy compliance |

## Role assignment

Initial assignments are documented in `okf/governance/roles.md`. Role changes follow the
change governance process (§35).
