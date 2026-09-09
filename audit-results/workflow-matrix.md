# Workflow matrix — 2026-09-09

| Workflow | Entry | Terminal effect | Primary proof | Residual boundary |
| --- | --- | --- | --- | --- |
| Agent request | MCP/HTTP adapter | journal, route, commit | control-flow artifact; unit tests | live stack |
| Retrieval | `/v1/retrieve` | tenant-scoped database reads | authorization artifact | identity binding must be revalidated in deployment |
| Tool execution | MCP → execution proxy | governed subprocess execution | control-flow artifact; tests | host policy and operator approval |
| Promotion | consolidation worker | OKF proposal/approval | business-logic and persistence artifacts | human review and Git remote |
| Recovery | recovery service | lease/checkpoint restoration | workflow/error artifacts | restore drill and off-host backup |
