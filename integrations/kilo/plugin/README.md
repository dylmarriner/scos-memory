# SCOS Kilo Code plugin

Install this directory with Kilo's plugin mechanism, then add the companion
SCOS MCP server from [`../mcp.json`](../mcp.json). The plugin supplies the
Kilo-native prompt hook; MCP supplies the seven governed SCOS tools (scoped
to that memory tier — `mcp-gateway` does not expose code-intelligence or
governance tools to the `kilo` agent identity).

Unlike the skill-based bundles (Claude Code, Codex, OpenCode), this plugin
has no session-start hook and no slash commands — Kilo's plugin API only
exposes a `chat.message` hook, so the reminder in `index.js` fires on every
message rather than once per session. It's deliberately kept to one line
for that reason.

Kilo's Architect/Code/Ask/Debug modes are handoffs within one task, not
separate sessions — memory recalled in one mode is expected to still be
relevant after switching modes on the same task, and the reminder text says
so explicitly.
