# OpenClaw integration

Native OpenClaw plugin (`plugin/`) that connects the SCOS Memory Fabric to the
OpenClaw agent runtime. Two **additive** surfaces — SCOS never claims the
exclusive `plugins.slots.memory` slot (on this fleet that stays with
`openclaw-honcho`):

1. **Memory corpus + prompt supplement.** `registerMemoryCorpusSupplement`
   makes OpenClaw's own `memory_search` reach the SCOS corpus, and
   `registerMemoryPromptSupplement` adds a short SCOS guidance line to the
   memory prompt section. Both are non-exclusive, so SCOS and the active memory
   plugin run side by side.
2. **Governed native tools** that call mcp-gateway directly (JSON-RPC
   `tools/call`), with timeout, bounded retry, idempotent writes, and metrics.

## Tools

Read (always available):

| Tool                    | Purpose                                             |
| ----------------------- | --------------------------------------------------- |
| `scos_memory_guidance`  | Explain the SCOS workflow for the current task      |
| `scos_recall`           | Query-specific prior context                        |
| `scos_status`           | Gateway provider health/reachability                |
| `scos_health`           | Local integration metrics (calls, retries, errors)  |
| `scos_explain`          | Why a memory is held, with evidence                 |
| `scos_fact_history`     | Append-only revision history of a fact              |
| `scos_memory_summary`   | Summarize active facts (LLM or deterministic digest)|
| `scos_user_model`       | Stored user representation                          |
| `scos_memory_graph`     | Memory entities and relationships                   |
| `scos_context_compile`  | Compile input into a bounded context capsule        |

Write (opt-in, `allowWrites: true`):

| Tool                 | Purpose                                              |
| -------------------- | ---------------------------------------------------- |
| `scos_fact_feedback` | Quality signal (POSITIVE/NEGATIVE/VERY_NEGATIVE)     |
| `scos_correct`       | Correct a memory by exact `memory_id`, keeping history |
| `scos_forget`        | Remove a memory by exact `memory_id`                 |

Writes are off by default because they mutate stored memory and need an exact
`memory_id` confirmed first. Read tools and `memory_search` are unaffected.

## Configuration

Credentials/identity resolve in this precedence order:
plugin config → process env → `~/.hermes/scos.env` → defaults.

| Config key               | Default | Notes                                             |
| ------------------------ | ------- | ------------------------------------------------- |
| `gatewayUrl`             | env     | Falls back to `SCOS_GATEWAY_URL`                  |
| `bearerToken`            | env     | Falls back to `SCOS_BEARER_TOKEN`                 |
| `tenantId` / `principalId` | env   | Falls back to `SCOS_TENANT_ID` / `SCOS_PRINCIPAL_ID` |
| `agentName`              | openclaw| `X-SCOS-Agent-Name` header                        |
| `tlsCaPath` / `tlsInsecureSkipVerify` | – | TLS verification                  |
| `requestTimeoutMs`       | 8000    | Per-attempt timeout                               |
| `recallTimeoutMs`        | 6000    | Recall/supplement timeout                         |
| `maxRetries`             | 2       | Retries for transient gateway failures            |
| `enableCorpusSupplement` | true    | Include SCOS in `memory_search`                   |
| `enablePromptSupplement` | true    | One-line SCOS guidance in the memory prompt       |
| `promptSummary`          | false   | Inject a cached SCOS fact summary pre-turn        |
| `promptSummaryTtlMs`     | 300000  | TTL for that cached summary                       |
| `allowWrites`            | false   | Expose the write tools                            |

Set it in `openclaw.json`:

```json
{
  "plugins": {
    "entries": {
      "scos-memory": {
        "enabled": true,
        "config": { "agentName": "openclaw", "enableCorpusSupplement": true }
      }
    },
    "load": { "paths": ["/media/kubuntux/DEVELOPMENT1/scos-memory-fabric/integrations/openclaw/plugin"] }
  }
}
```

## Build, test, verify

```bash
cd plugin
npm install
npm run plugin:build      # tsc + regenerate openclaw.plugin.json
npm run plugin:validate   # prints "Plugin scos-memory is valid."
npm test                  # node --test

../scripts/smoke.sh       # live gateway smoke test (read-only)
```

After a plugin change, restart the Gateway so the rebuilt `dist/index.js`
loads.

## Failure behaviour

SCOS failures fail **open**: a corpus supplement error degrades to empty
results and never breaks `memory_search`, and a gateway outage never blocks an
agent turn. `scos_health` reports counters, retries, and the last error. Raw
errors surface in tool results for explicit `scos_*` calls.

## Skills

`skills/scos-recall/`, `skills/scos-explain/`, `skills/scos-status/` are
user-invocable skills (OpenClaw skill format), copied to `~/.openclaw/skills/`
by `scripts/install_agent_plugins.py`. They are additive to the native tools.
