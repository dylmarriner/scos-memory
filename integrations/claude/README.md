# Claude Code + Claude Cowork + Claude Desktop integrations

Packaged installers for connecting Claude to a SCOS Memory Fabric
`mcp-gateway` service, instead of the manual `~/.claude.json` / `claude mcp
add` steps in `docs/agent-connection-guide.md`. None bakes in a default
server — each is configured against whatever `mcp-gateway` deployment you
point it at (self-hosted or otherwise) — and all three expose the same core
tools: `scos_recall`, `scos_status`, `scos_explain`, `scos_user_model`,
`scos_memory_graph`, `scos_correct`, `scos_forget`.

| | Claude Code | Claude Cowork | Claude Desktop |
|---|---|---|---|
| Directory | `plugins/scos-memory-fabric/` | `plugins/scos-memory-fabric-cowork/` | `desktop-extension/` |
| Mechanism | Plugin, installed via this dir's `.claude-plugin/marketplace.json` | Same marketplace, `.plugin` install | `.mcpb` bundle (Desktop Extension) |
| Transport | Native `http` MCP transport, direct to `mcp-gateway` | Native `http` MCP transport, direct to `mcp-gateway` | Bundled local stdio↔HTTP relay (`server/index.js`), because Desktop only launches local stdio servers |
| Extras | Slash commands + `SessionStart` hook + full skill (incl. graphify/code-intelligence tools) | Single skill, memory tools only — no commands/hooks (Cowork convention) | None — raw MCP connection |
| Secrets | `userConfig`, prompted on `/plugin install`, sensitive fields in OS keychain | `userConfig`, prompted on install, sensitive fields in OS keychain | `user_config`, prompted on `.mcpb` install, sensitive fields in OS keychain |

All three need the same four values — gateway URL, bearer token, tenant ID,
principal ID — from whoever runs your SCOS Memory Fabric deployment (see
`docs/agent-connection-guide.md` §2 for what each one is). None of the
packagings embed a token — see each subdirectory's README for exact install
steps.

- [`plugins/scos-memory-fabric/README.md`](plugins/scos-memory-fabric/README.md)
- [`plugins/scos-memory-fabric-cowork/README.md`](plugins/scos-memory-fabric-cowork/README.md)
- [`desktop-extension/README.md`](desktop-extension/README.md)

## Quick start

```bash
# Claude Code
claude
/plugin marketplace add ./integrations/claude
/plugin install scos-memory-fabric@scos-fabric-tools

# Claude Cowork
# In the Cowork app: Add plugin from ./integrations/claude,
# then install scos-memory-fabric-cowork

# Claude Desktop
npm install -g @anthropic-ai/mcpb
(cd integrations/claude/desktop-extension && mcpb pack)
# then double-click the resulting scos-memory-fabric.mcpb
```
