# OpenClaw integration

Merge `mcp.json` into `~/.openclaw/mcp.json` under `mcpServers`, then restart
OpenClaw. The bridge reads credentials from `~/.hermes/scos.env`.

`skills/scos-recall/`, `skills/scos-explain/`, `skills/scos-status/` are
user-invocable skills, each a directory with its own `SKILL.md` (OpenClaw's
skill format). `scripts/install_agent_plugins.py` copies them to
`~/.openclaw/skills/`, OpenClaw's documented local-override directory.
User-invocable skills are reachable as `/skill scos-recall [query]`, and
possibly as a direct `/scos-recall` if this OpenClaw build auto-registers
user-invocable skills as their own command — verify against `/tools/skills`
in your installed version, since the exact frontmatter schema wasn't
confirmed against a live install here.

`plugin/src/index.ts` (the existing `defineToolPlugin`) is a separate,
already-working mechanism — a governed tool (`scos_memory_guidance`), not a
slash command. The skills above are additive, not a replacement for it.
