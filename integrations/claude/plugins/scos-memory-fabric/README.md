# scos-memory-fabric (Claude Code plugin)

Wires Claude Code to the SCOS Memory Fabric's `mcp-gateway` service
(`services/mcp-gateway`, deployed at
`https://rabuntu.tail2c15c8.ts.net:8098/mcp` — see
`docs/agent-connection-guide.md`) over the native `http` MCP transport. No
local bridge script, no CA trust setup: `mcp-gateway` serves a
publicly-trusted (Let's Encrypt via Tailscale) cert, so this works from any
machine that can reach `rabuntu.tail2c15c8.ts.net:8098`.

Tools exposed once connected: `scos_recall`, `scos_status`, `scos_explain`,
`scos_user_model`, `scos_memory_graph`, `scos_correct`, `scos_forget` (the
last two currently return `{"implemented": false}` — see the connection
guide).

## Install

From this repo (or any checkout that has `integrations/claude/`):

```bash
claude
/plugin marketplace add ./integrations/claude
/plugin install scos-memory-fabric@scos-fabric-tools
```

Claude Code prompts for four values on enable — the same ones documented in
`docs/agent-connection-guide.md` §2, read from your own `~/.hermes/scos.env`:

| Prompt | Source |
|---|---|
| mcp-gateway URL | defaults to the rabuntu deployment; override only if pointed at a different environment |
| SCOS bearer token | `SCOS_HERMES_BEARER_TOKEN` in `~/.hermes/scos.env` |
| Tenant ID | `SCOS_TENANT_ID` in `~/.hermes/scos.env` |
| Principal ID | `SCOS_PRINCIPAL_ID` in `~/.hermes/scos.env` |

The bearer token is marked `sensitive` — Claude Code stores it in your OS
keychain (or `~/.claude/.credentials.json` where no keychain is available),
never in plaintext `settings.json`, and never in this repo.

Verify with `/mcp` inside a session, or:

```bash
claude mcp list
```

`scos` should show `✔ Connected`.

## Reconfigure or remove

```bash
claude plugin disable scos-memory-fabric@scos-fabric-tools
claude plugin enable scos-memory-fabric@scos-fabric-tools   # re-prompts for config
claude plugin uninstall scos-memory-fabric@scos-fabric-tools
```

## What the plugin adds beyond the raw MCP connection

- **Slash commands** (`commands/`): `/scos-recall <query>`, `/scos-status`,
  `/scos-explain <memory_id>` — thin wrappers around the read-path tools for
  quick invocation.
- **Skill** (`skills/scos-memory/`): teaches Claude when to reach for each
  of the seven tools (e.g. call `scos_recall` before answering from
  assumption about prior context) and to always confirm the exact
  `memory_id` with the user before attempting `scos_correct` or
  `scos_forget`, since those mutate memory and aren't implemented on this
  fleet yet.
- **Hook** (`hooks/hooks.json`): a `SessionStart` hook that primes new
  sessions with a short reminder of the above, so the behavior isn't only
  as good as whether the skill happens to get pulled in.

## Why a plugin instead of editing `~/.claude.json` / `.mcp.json` by hand

Same effect as the `claude mcp add --transport http scos-remote ...` command
in `docs/agent-connection-guide.md`, minus retyping the URL and headers on
every machine and minus the bearer token ever touching a config file that
could get synced or committed by mistake.
