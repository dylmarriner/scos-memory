# Independent completeness passes

1. **Attacker inputs:** independently rebuilt the map from headers, JSON commands,
IDs, manifests, operation results, environment, persisted rows, and external
responses. This found result/history/raw-output disclosure and confirmed header
equality is not issuer authentication.
2. **State/business logic:** independently modeled tool, extension, recovery,
backup, and execution-proxy transitions. This found caller-chosen operation-ID
overwrite and unrestricted control-plane completion/reconciliation paths.
3. **Darkest corners:** independently sampled adapters, compatibility, health and
operational routes, migrations/RLS, maintenance workers, Docker topology, and
fallbacks. This confirmed the weak default database password and identified the
absence of a single service-authentication boundary as the dominant residual risk.

Each pass used an independent question. No full-coverage claim is made.
