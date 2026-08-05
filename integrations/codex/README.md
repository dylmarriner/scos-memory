# Codex integration

Packaged Codex CLI plugin for connecting to the SCOS Memory Fabric,
instead of the manual `~/.codex/config.toml` steps in
`docs/agent-connection-guide.md` §3. Same backend and tools as the
Claude Code / Claude Desktop packaging in `../claude/`:
`scos_recall`, `scos_status`, `scos_explain`, `scos_user_model`,
`scos_memory_graph`, `scos_correct`, `scos_forget`.

- [`plugins/scos-memory-fabric/README.md`](plugins/scos-memory-fabric/README.md)

## Quick start

```bash
codex plugin marketplace add ./integrations/codex
codex plugin add scos-memory-fabric@scos-fabric-tools
codex mcp list   # should show `scos`
```

## Layout

Codex plugins follow a different manifest convention than Claude Code
plugins (confirmed against the `plugin-creator` skill bundled with Codex
CLI 0.146.0, and by round-tripping `codex plugin marketplace add` /
`codex plugin add` / `codex plugin remove` against this exact directory):

- `.agents/plugins/marketplace.json` — marketplace manifest (this is the
  Codex-specific location; Claude Code's equivalent is
  `.claude-plugin/marketplace.json` at the same relative spot).
- `plugins/scos-memory-fabric/.codex-plugin/plugin.json` — plugin
  manifest.

Both marketplaces (`../claude/.claude-plugin/marketplace.json` and this
one) point at the same backend, packaged separately because Claude Code
and Codex don't share a plugin format.
