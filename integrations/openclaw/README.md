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
already-working mechanism — native OpenClaw tools, not slash commands. The
skills above are additive, not a replacement for it. Besides the original
`scos_memory_guidance` tool, it now also exposes `scos_recall`, `scos_status`,
and `scos_explain` as native tools that POST directly to mcp-gateway (same
JSON-RPC `tools/call` protocol and `~/.hermes/scos.env` credentials as
`scos-mcp-bridge.py`), so those three work even without the `mcp.json` bridge
configured. `scos_correct` and `scos_forget` are deliberately left out of the
native plugin — they mutate stored memory, aren't implemented on this fleet
yet, and need an exact `memory_id` confirmed with the user first, which isn't
a good fit for a tool an agent can call on its own.

Run `npm install && npm run build` inside `plugin/` to produce `dist/index.js`
before OpenClaw loads the extension.

To publish this plugin to ClawHub (`clawhub package publish <source>`), fill
in the real `openclaw.compat.pluginApi` and `openclaw.build.openclawVersion`
values in `plugin/package.json` first — they're placeholders here because
ClawHub's docs didn't publish example version strings and the actual values
depend on the OpenClaw build you're targeting (check your installed
`openclaw` package version). Use `clawhub package publish --dry-run` to
validate before uploading.
