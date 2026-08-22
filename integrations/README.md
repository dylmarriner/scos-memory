# Agent integrations

The complete local-agent catalog is in
[`agent-plugins/`](agent-plugins/), including native bundles for Hermes,
Gemini, Cline, Cursor, Windsurf/Devin, Antigravity, Continue, and Aider's
rules-only fallback. Run `python3 integrations/agent-plugins/validate.py` to
check that every registered local agent has a bundle. Each agent uses its own
`SCOS_AGENT_NAME` while sharing the same secret-free bridge configuration.

Packaged installers for connecting coding agents to the SCOS Memory
Fabric, instead of the manual per-agent steps in
`docs/agent-connection-guide.md`. All agents point at the same memory
backend and code index: the bridge exposes the seven memory tools
(`scos_recall`, `scos_status`, `scos_explain`, `scos_user_model`,
`scos_memory_graph`, `scos_correct`, `scos_forget`) plus three graphify
knowledge-graph tools (`scos_graphify_query`, `scos_graphify_path`,
`scos_graphify_explain`) and the six code intelligence tools
(`scos_code_repo_register`, `scos_code_repo_list`, `scos_code_repo_get`,
`scos_code_index`, `scos_code_query`, `scos_code_impact`), while the
`mcp-gateway` http transport exposes the full 139-tool governed surface.

| | Claude Code | Claude Desktop | Codex CLI | Hermes Agent | Kilo | OpenCode | OpenClaw | Qwen Code |
|---|---|---|---|---|
| Directory | [`claude/plugins/scos-memory-fabric/`](claude/plugins/scos-memory-fabric/) | [`claude/desktop-extension/`](claude/desktop-extension/) | [`codex/plugins/scos-memory-fabric/`](codex/plugins/scos-memory-fabric/) | [`hermes/plugins/scos-memory/`](hermes/plugins/scos-memory/) | [`kilo/`](kilo/) | [`opencode/dot-opencode/`](opencode/dot-opencode/) | [`openclaw/`](openclaw/) | [`qwen/`](qwen/) |
| Mechanism | Plugin, via `claude/.claude-plugin/marketplace.json` | `.mcpb` bundle (Desktop Extension) | Plugin, via `codex/.agents/plugins/marketplace.json` | Copy into `.opencode/` — no marketplace system exists for local bundles |
| Backend | `mcp-gateway` (`services/mcp-gateway`), Streamable HTTP | `mcp-gateway`, via bundled stdio↔HTTP relay | `hermes-provider` directly, via bundled stdio bridge | `hermes-provider` directly, via bundled stdio bridge |
| Transport | Native `http` MCP transport | Local stdio, relayed to HTTP (Desktop only launches local stdio servers) | Local stdio (Codex plugin `.mcp.json` local-command entries, same as its manual config) | Local stdio (`mcp.json`, same as its manual config) |
| Secrets | `userConfig`, prompted on `/plugin install` | `user_config`, prompted on `.mcpb` install | Read from `~/.hermes/scos.env` at runtime (no per-install prompt confirmed for Codex plugins) | Read from `~/.hermes/scos.env` at runtime (OpenCode has no plugin-install secret prompt) |
| Extras | Slash commands (`/scos-recall`, `/scos-status`, `/scos-explain`), a `scos-memory` skill, `SessionStart` hook | — (Desktop extensions only support the MCP server itself) | Same `scos-memory` skill, `SessionStart` hook | Same `scos-memory` skill, a `scos-memory-reminder.ts` behavior plugin (`session.created` + `chat.system.transform` hooks) |

Claude's two packagings go through `mcp-gateway`; Codex and OpenCode talk
straight to `hermes-provider`, matching what was already manually
configured for them on this fleet (`docs/agent-connection-guide.md` §3).
Functionally equivalent — same tools, same underlying memory store.

Each subdirectory has its own README with exact install/remove steps:

- [`claude/README.md`](claude/README.md)
- [`codex/README.md`](codex/README.md)
- [`opencode/README.md`](opencode/README.md)
