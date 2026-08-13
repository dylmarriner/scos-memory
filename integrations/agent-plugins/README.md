# SCOS agent plugin catalog

This directory records the agent-specific SCOS integration contract for every
local coding agent in the fleet. Each entry has its own plugin bundle and
uses a distinct `SCOS_AGENT_NAME`, so memory sessions do not collide.

The bundles all use the same secret-free contract:

- the agent launches the local stdio MCP bridge;
- the bridge reads `~/.hermes/scos.env` and the local CA at runtime;
- the plugin's instructions require `scos_recall` when prior context matters;
- `scos_correct` and `scos_forget` require confirmation of the exact
  `memory_id` first.

Hermes Agent is the exception: its native Python plugin calls
`hermes-provider` directly and is kept at
`integrations/hermes/plugins/scos-memory/`.

Some desktop agents do not expose a plugin SDK. Their bundle is therefore the
agent's native MCP/rules package, not a fabricated plugin manifest. The
`registry.json` file is the source of truth for that distinction.

## Local installation

The bundles are intentionally portable and do not write credentials. Install
the relevant bundle using its README, then restart the agent. The bridge must
already be available at `~/.hermes/scripts/scos-mcp-bridge.py` and the shared
`~/.hermes/scos.env` must be configured.

