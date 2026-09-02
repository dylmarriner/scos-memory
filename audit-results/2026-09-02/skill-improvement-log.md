# Skill improvement log — 2026-09-02

- Compare schema comments and capability claims against the exact write path; comments describing “stable” or “immutable” behavior are not evidence.
- Treat client-supplied manifests as untrusted state descriptions and require a server-verifiable completeness proof before activating an index.
- For RLS audits, inspect the actual runtime role/bootstrap, not only policy SQL and fixture-created test roles.
