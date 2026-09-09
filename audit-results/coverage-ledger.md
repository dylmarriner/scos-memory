# Current audit coverage — 2026-09-09

| Surface | Evidence | Status |
| --- | --- | --- |
| Go services and shared packages | Existing specialist artifacts plus current tree inventory | Reviewed; re-run of every specialist was not possible because the referenced specialist skill files are absent from this checkout |
| Authentication and authorization | `audit-results/hostile-authentication-identity-auditor.md`, `hostile-authorization-auditor.md` | Prior evidence carried forward; current deployment state unverified |
| Persistence and RLS | `hostile-persistence-auditor.md`, `docs/security/threat-model.md` | Source-reviewed; live database proof deployment-owned |
| Workflows and recovery | `hostile-workflow-state-machine-auditor.md`, `hostile-error-path-auditor.md` | Source-reviewed; live failure-injection must be run against Compose |
| Integrations and supply chain | `hostile-integration-auditor.md`, `hostile-dependency-supply-chain-auditor.md` | Source-reviewed; external endpoints and provider state unverified |
| Documentation, CI, and community surface | Root files, `.github/`, `docs/`, `Makefile` | Current-turn reviewed |

Existing audit artifacts are retained. This ledger does not convert old evidence
into current runtime evidence.
