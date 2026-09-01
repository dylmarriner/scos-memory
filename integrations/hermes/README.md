# Hermes Agent integration

Two plugins live here. **Use `plugins/scos` — it supersedes `plugins/scos-memory`.**

Both share the same connection file, `~/.hermes/scos.env`, and both now
default to real TLS certificate verification and require
`SCOS_BEARER_TOKEN`, `SCOS_TENANT_ID`, and `SCOS_PRINCIPAL_ID` to be set —
a misconfigured plugin raises clearly instead of silently sending broken
auth or skipping certificate checks. For local development only, set
`SCOS_TLS_INSECURE_SKIP_VERIFY=1` explicitly; never use that setting in
production.

## `plugins/scos` — MemoryProvider (persistent memory, use this one)

A first-class Hermes `MemoryProvider`: it plugs into the same
prefetch-before-turn / sync-after-turn lifecycle as the bundled
mem0/honcho/hindsight providers, so SCOS Nexus recall and persistence happen
automatically — no explicit tool call required — the same guarantee the
Claude Code and Codex integrations get from their `scos-auto-recall` /
`scos-auto-persist` hooks.

What it does each turn:
- **`prefetch()`** — calls `scos_recall` on mcp-gateway before the model sees
  the prompt, and injects the result into context.
- **`sync_turn()`** — after the turn, writes the user+assistant exchange to
  `hermes-provider`'s `/v1/provider/memory_write` (background thread,
  non-blocking), mirroring what Claude Code's Stop hook does.
- **`get_tool_schemas()` / `handle_tool_call()`** — still exposes every tool
  mcp-gateway reports (`scos_recall`, `scos_status`, etc.) for the model to
  call explicitly when it needs something the automatic recall missed.

Install:

```bash
cp -R integrations/hermes/plugins/scos ~/.hermes/plugins/scos
```

Then activate it as the active memory provider (Hermes allows only one
external provider at a time — see `agent/memory_provider.py`):

```bash
hermes memory setup   # pick "scos", or set memory.provider: scos in config.yaml directly
```

| Key | Required for | Purpose |
| --- | --- | --- |
| `SCOS_GATEWAY_URL` | recall + tools | mcp-gateway endpoint |
| `SCOS_BEARER_TOKEN` | recall + tools | mcp-gateway auth |
| `SCOS_TENANT_ID` / `SCOS_PRINCIPAL_ID` | recall + tools | tenant scoping headers |
| `SCOS_HERMES_PROVIDER_URL` | turn persistence | hermes-provider base URL |
| `SCOS_HERMES_BEARER_TOKEN` | turn persistence | hermes-provider auth |
| `SCOS_NAMESPACE_ID` | turn persistence | namespace for written turns |

Persistence is opt-in in effect: if the `SCOS_HERMES_PROVIDER_URL` /
`SCOS_HERMES_BEARER_TOKEN` / `SCOS_NAMESPACE_ID` trio is absent, `sync_turn()`
silently no-ops and you still get recall-only behavior.

## `plugins/scos-memory` — toolset plugin (superseded)

The original integration: registers every mcp-gateway tool generically via
`ctx.register_tool()`, with no automatic prefetch/persist — the model has to
choose to call `scos_recall` itself, which is exactly the gap `plugins/scos`
closes. It reads the existing `~/.hermes/scos.env` connection settings and
discovers the governed SCOS tool catalogue after Hermes restarts, so memory,
graph, code-intelligence, and future tools are available through one
tenant-scoped plugin.

The plugin intentionally does not declare `requires_env`: Hermes does not
inherit values from `~/.hermes/scos.env` during plugin discovery, while the
plugin itself safely loads that file at call time.

Kept for reference / for agents that only need occasional explicit tool
access without adopting SCOS as the memory provider. **Don't install
both** — they'd double-register the same `scos_recall`/`scos_status`/etc.
tool names against the same gateway, once as memory-provider tools, once as
generic plugin tools.

```bash
cp -R integrations/hermes/plugins/scos-memory ~/.hermes/plugins/scos-memory
```

No `/scos-recall`-style slash commands here: Hermes Agent is internal to
this fleet, not a public product, and `plugin.yaml` doesn't expose a
command-registration surface the way the Claude Code / Codex / OpenCode
plugin schemas do — only tool/provider registration.
