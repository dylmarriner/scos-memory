# Three completeness passes — 2026-09-02

1. Inputs-to-sinks: found caller-controlled snapshot manifests, identity fallbacks, Graphify subprocess/cache, and Graphify-to-memory metadata loss.
2. State/business logic: found wrong symbol count, unstable ordinary-edit identity, and activation without completeness proof.
3. Dark corners: found v1/v2 divergence, runtime RLS-role gap, parse-error partial success, and unverified cache write atomicity.

Each pass added findings not established by the previous pass; live topology and database-role behavior remain explicit gaps.
