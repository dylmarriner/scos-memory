# Workflow matrix

| Workflow | Actor | Entry | Trust boundary | State/effects | Abuse tested | Result |
|---|---|---|---|---|---|---|
| execution plan/execute | caller/operator | execution-proxy HTTP | network to shell | process/raw file/result map | spoof headers/arbitrary command/duplicate ID | confirmed candidate |
| auth issue/validate/revoke | caller/service | security-gate HTTP | network to authority | in-memory token | arbitrary identity/missing issuer proof | confirmed candidate |
| tool lifecycle | caller/service | tool-governance HTTP | network to admin state | in-memory tool/op | valid mutation/replay | high-confidence auth gap |
| extension install | caller/service | extension-manager HTTP | network to registry | JSON state/hooks/skills | signature-field spoof/partial persist | high-confidence auth gap |
| route decision | caller/gateway | control-plane HTTP | headers to PostgreSQL/RLS | runs/decisions | spoof/replay/duplicate interaction | boundary-dependent |
| memory journal | principal | memory-gateway HTTP | headers to PostgreSQL/RLS | sessions/turns/outbox | tenant mismatch | local checks; integration gap |
| consolidation | worker | outbox polling | DB to worker | candidates/memory | duplicate/retry/lease | unit-reviewed; live gap |

