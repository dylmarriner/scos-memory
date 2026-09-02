# Assumption register — 2026-09-02

- Internal service headers are assumed unreachable to untrusted callers; deployment proof absent.
- A caller-supplied file manifest is assumed complete; no server-side Git tree proof exists.
- Graphify output is assumed fully written before cache hit; no inter-process lock/atomic output proof exists.
- SCIP is assumed to be produced by a trusted, correct indexer; artifact provenance/signature is not verified.
- RLS is assumed effective in deployment; runtime role and FORCE RLS are not present in inspected migrations.
- Existing tests are assumed to cover current behavior; benchmark and live integration evidence are absent.
