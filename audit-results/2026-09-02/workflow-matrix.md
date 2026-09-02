# Workflow matrix — 2026-09-02

| Workflow | Entry | State/effect | Main risk |
|---|---|---|---|
| Memory | `/v1/facts`, retrieval, worker | candidate/version/active/forgotten | temporal/derived deletion proof |
| Code index | `/v2/code/index` | snapshot → ready → active | partial manifest, wrong count, unstable IDs |
| SCIP | `/v2/code/index/scip` | symbols/edges in snapshot | external indexer availability |
| Graphify | extract/query/path/explain | cache artifact + subprocess | concurrent output, provenance loss |
| Auth/RLS | middleware + SQL settings | identity → scoped query | runtime role/FORCE RLS gap |
