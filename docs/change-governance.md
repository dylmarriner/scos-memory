# Change governance — Architecture Decision Records (§35)

Material changes to SCOS require an approved ADR before implementation.

## When an ADR is required

- Authoritative storage changes
- Event semantic changes
- Taxonomy changes
- Permission model changes
- Retention policy changes
- Encryption changes
- Provider or broker changes
- Database changes
- Embedding model migration
- Shared scope changes
- Autonomous authority changes
- Ingress interception changes
- Capability registry contract changes
- Route policy changes
- Confidence threshold changes
- Token-optimisation policy changes
- Code-index schema changes
- Proxy rewrite policy changes
- Learning signal changes
- Superiority benchmark changes

## ADR requirements

Every ADR MUST define:

1. **Compatibility** — backwards, forwards, or breaking
2. **Migration** — automated or manual
3. **Rollback** — strategy and verification
4. **Index rebuild** — if required
5. **Verification** — how to confirm success

Security-critical changes (permissions, encryption, secrets, authentication)
require explicit approval from the **System owner** and **Security steward**.

## Process

1. Proposal: draft ADR in `docs/adr/`
2. Validation: review against GOVERNANCE.md
3. Diff: show specific changes
4. Approval: authorised roles sign off
5. Implementation: merge to protected branch
6. Indexing: OKF synchronised
