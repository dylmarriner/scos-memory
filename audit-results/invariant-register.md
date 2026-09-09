# Invariant register — 2026-09-09

- Authority is PostgreSQL for durable conversational state and OKF Git for approved knowledge.
- Tenant and principal identity must be cryptographically bound before tenant-scoped effects.
- Mutations require explicit lifecycle state and idempotency where replay is possible.
- Derived indexes and embeddings must be rebuildable without becoming authorities.
- Missing production secrets and unsafe TLS configuration must fail closed.
- Local compilation, structural audits, and unit tests do not establish deployment readiness.

Open verification: repeat the authorization and RLS checks against the running
Compose stack before treating the first two invariants as operationally proven.
