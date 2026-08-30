# Claude Code + Claude Desktop integrations

Packaged installers for connecting Claude to the SCOS Memory Fabric's
hosted gateway (`cloud-control-plane` fronting `mcp-gateway`), instead of
the manual `~/.claude.json` / `claude mcp add` steps in
`docs/agent-connection-guide.md`. Both point at the same backend
(`https://scos.172.236.155.15.nip.io/mcp` by default) and expose the same
tools: `scos_recall`, `scos_status`, `scos_explain`, `scos_user_model`,
`scos_memory_graph`, `scos_correct`, `scos_forget`.

| | Claude Code | Claude Desktop |
|---|---|---|
| Directory | `plugins/scos-memory-fabric/` | `desktop-extension/` |
| Mechanism | Plugin, installed via this dir's `.claude-plugin/marketplace.json` | `.mcpb` bundle (Desktop Extension) |
| Transport | Native `http` MCP transport, direct to `mcp-gateway` | Bundled local stdio↔HTTP relay (`server/index.js`), because Desktop only launches local stdio servers |
| Secrets | `userConfig`, prompted on `/plugin install`, sensitive fields in OS keychain | `user_config`, prompted on `.mcpb` install, sensitive fields in OS keychain |

Both need the gateway URL (default is fine unless you're pointed at a
different environment) and your own per-user API key (`scos_live_...`,
issued at signup or listed in the SCOS portal under Settings → API Keys —
also available as `SCOS_BEARER_TOKEN` in your own `~/.hermes/scos.env` per
`docs/agent-connection-guide.md` §2). The tenant/principal ID fields are
legacy and no longer required — the gateway derives your identity from the
API key itself. Neither packaging embeds a token — see each subdirectory's
README for exact install steps.

- [`plugins/scos-memory-fabric/README.md`](plugins/scos-memory-fabric/README.md)
- [`desktop-extension/README.md`](desktop-extension/README.md)

## Quick start

```bash
# Claude Code
claude
/plugin marketplace add ./integrations/claude
/plugin install scos-memory-fabric@scos-fabric-tools

# Claude Desktop
npm install -g @anthropic-ai/mcpb
(cd integrations/claude/desktop-extension && mcpb pack)
# then double-click the resulting scos-memory-fabric.mcpb
```
