# Invariant register

- Cloud publication is limited to intended ingress and readiness probes the actual listener: fixed in current source, Compose rendering pending.
- Every browser mutation has CSRF proof: not enforced; PR-003.
- Team membership never exceeds `max_members`: lock fix applied; integration proof pending.
- Tenant isolation survives database access: unproven; PR-005.
- High-impact execution requires independent approval and is idempotent: contradicted by inherited artifacts; PR-006.
- Authoritative governance/recovery state survives restart: contradicted by inherited artifacts; PR-007.
