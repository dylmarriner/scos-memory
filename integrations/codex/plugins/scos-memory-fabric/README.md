# scos-memory-fabric (Codex plugin)

Wires Codex CLI to the SCOS Memory Fabric via a bundled, self-contained
stdio bridge (`scripts/scos-mcp-bridge.py`, a copy of
`~/.hermes/scripts/scos-mcp-bridge.py`) that relays to the public MCP
gateway on the Linode VPS — `https://scos.172.236.155.15.nip.io/mcp`
(account API key `scos_live_*`, 146 governed tools).

Codex plugin `.mcp.json` entries (unlike Claude Code's) weren't confirmed to
support templating a bearer token into `headers` from a per-install
config prompt, so this follows the same pattern as the existing manual
Codex setup and the bundled Python bridge: it reads
`SCOS_GATEWAY_URL` / `SCOS_BEARER_TOKEN` / `SCOS_TENANT_ID` /
`SCOS_PRINCIPAL_ID` / `SCOS_TLS_CA_PATH`
from `~/.hermes/scos.env` on the machine it runs on,
same as every other agent on this fleet. The bridge's baked-in defaults
point at the Linode gateway and Dylan's workspace identity
(tenant 8e2edc7a-..., principal 6a11c3f9-...).

Tools exposed once connected: `scos_recall`, `scos_status`, `scos_explain`,
`scos_user_model`, `scos_memory_graph`, `scos_correct`, `scos_forget`
(memory) plus `scos_graphify_query`, `scos_graphify_path`,
`scos_graphify_explain` (graphify knowledge-graph traversal) and
`scos_code_repo_register`, `scos_code_repo_list`, `scos_code_repo_get`,
`scos_code_index`, `scos_code_query`, `scos_code_impact` (code
intelligence).

## What the plugin adds

- **`.mcp.json`** — registers the `scos` stdio MCP server.
- **`skills/scos-memory/`** — teaches Codex when to reach for each tool and
  to confirm the exact `memory_id` before `scos_correct`/`scos_forget`.
- **`hooks.json`** — a `SessionStart` hook that reminds new sessions of the
  same guidance via `additionalContext`.
- **`prompts/scos-recall.md`, `prompts/scos-explain.md`, `prompts/scos-status.md`**
  — `/scos-recall`, `/scos-explain`, `/scos-status` custom prompts. Codex's
  plugin manifest has no field to bundle these directly, so
  `scripts/install_agent_plugins.py` copies them into `$CODEX_HOME/prompts/`
  (`~/.codex/prompts/`) separately from the `codex plugin add` step.

## Prerequisite

`~/.hermes/scos.env` must already exist on the machine (mode `600`, holds
the bearer token — never commit it), per
`docs/agent-connection-guide.md` §2. This is a one-time, per-machine setup
shared by every agent on the fleet, not something this plugin provisions.

## Install

```bash
codex plugin marketplace add ./integrations/codex
codex plugin add scos-memory-fabric@scos-fabric-tools
```

Verify:

```bash
codex mcp list
```

**Note:** if this machine already has `scos` configured manually in
`~/.codex/config.toml` (the setup documented in
`docs/agent-connection-guide.md` §3), that entry and this plugin's entry
share the same server name (`scos`) and only one will show. Remove the
manual `[mcp_servers.scos]` block from `config.toml` if you want the
plugin to be the sole source, or skip installing this plugin if the manual
config already works for you — they're equivalent.

## Remove

```bash
codex plugin remove scos-memory-fabric@scos-fabric-tools
codex plugin marketplace remove scos-fabric-tools
```

## Why bundle a copy of the bridge script instead of pointing at `~/.hermes/scripts/`

So the plugin installs cleanly on any teammate's machine from just this
repo checkout, without requiring `~/.hermes/scripts/scos-mcp-bridge.py` to
already exist at that exact path — mirroring why the Claude Desktop
extension (`integrations/claude/desktop-extension/`) bundles its own
relay instead of depending on a pre-placed script.
