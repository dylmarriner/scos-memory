# scos-memory-fabric (OpenCode integration)

OpenCode doesn't have a declarative plugin-manifest/marketplace system
like Claude Code or Codex — MCP servers are just config (`mcp.json` /
`opencode.jsonc`'s `"mcp"` key), and behavior plugins are plain
TypeScript files dropped into `.opencode/plugins/` that export a
`Plugin` implementing the hooks from `@opencode-ai/plugin`. So "install"
here means copying this directory's contents into `.opencode/`, not
running a marketplace command.

Connects to the SCOS Memory Fabric the same way the Codex packaging does
(`../codex/`): a bundled, self-contained stdio bridge
(`scripts/scos-mcp-bridge.py`, a copy of
`~/.hermes/scripts/scos-mcp-bridge.py`) talking directly to
`hermes-provider` on kubuntux and `code-intelligence` on :8084 — see
`docs/agent-connection-guide.md` §1/§3.

Tools exposed once connected: `scos_recall`, `scos_status`, `scos_explain`,
`scos_user_model`, `scos_memory_graph`, `scos_correct`, `scos_forget`
(memory) plus `scos_code_index`, `scos_code_query`, `scos_code_impact`
(code intelligence).

## What's here

- **`dot-opencode/mcp.json`** — registers the `scos` stdio MCP server.
  Schema confirmed against this machine's real
  `~/.opencode/mcp.json` (same `command`/`args`/`env`/`enabled` shape).
- **`dot-opencode/scripts/scos-mcp-bridge.py`** — the bundled bridge.
- **`dot-opencode/skills/scos-memory/`** — same skill content as the
  Claude Code / Codex packagings: when to call each tool, and to always
  confirm the exact `memory_id` before `scos_correct`/`scos_forget`.
- **`dot-opencode/plugins/scos-memory-reminder.ts`** — a behavior plugin
  using the `event` (`session.created`) and
  `experimental.chat.system.transform` hooks to inject the same
  memory-first reminder into the system prompt once per session (the
  OpenCode equivalent of the `SessionStart` hooks in the Claude Code and
  Codex packagings). Type-checked against the real `@opencode-ai/plugin`
  1.4.7 types installed on this machine.

## Prerequisite

`~/.hermes/scos.env` must already exist on the machine (mode `600`, holds
the bearer token — never commit it), per
`docs/agent-connection-guide.md` §2.

## Install

Project-scoped (checked into a repo's own `.opencode/`, shared with
teammates via that repo):

```bash
cp -r integrations/opencode/dot-opencode/. /path/to/your/project/.opencode/
```

Or global (applies to every project on this machine):

```bash
cp -r integrations/opencode/dot-opencode/. ~/.opencode/
```

Then verify:

```bash
opencode mcp list   # should show `scos`
```

**Path caveat:** `mcp.json`'s `args` uses a project-relative path
(`./.opencode/scripts/scos-mcp-bridge.py`), matching how OpenCode plugins
and tools resolve project-relative paths elsewhere in this fleet's
configs. Every other `mcp.json` entry seen on this machine uses an
absolute path instead, and a full `opencode mcp list` round-trip couldn't
be completed live here due to an unrelated local `opencode` version/DB
mismatch on this machine (nothing to do with this integration). If
`opencode mcp list` doesn't show `scos` as connected after copying these
files in, switch `args` to an absolute path to your project's
`.opencode/scripts/scos-mcp-bridge.py` and retry.

## Why bundle a copy of the bridge script instead of pointing at `~/.hermes/scripts/`

So the integration works from just this repo checkout on any teammate's
machine, without requiring `~/.hermes/scripts/scos-mcp-bridge.py` to
already exist at that exact path — same reasoning as
`../codex/plugins/scos-memory-fabric/README.md`.
