# scos-memory-fabric (Claude Code plugin)

Wires Claude Code to the SCOS Memory Fabric's hosted gateway
(`cloud-control-plane` fronting `services/mcp-gateway`, deployed at
`https://scos.172.236.155.15.nip.io/mcp` — see
`docs/agent-connection-guide.md`) over the native `http` MCP transport. No
local bridge script; the gateway serves the full governed tool surface (136
tools incl. the code indexer). The public endpoint has a real Let's Encrypt
certificate, so a plain `claude mcp add --transport http` with your API key
in the `Authorization` header (see the connection guide) is all that's
needed — no insecure/CA-pinning flags.

Tools exposed once connected: the full gateway set, incl. `scos_recall`,
`scos_status`, `scos_explain`, `scos_user_model`, `scos_memory_graph`,
`scos_correct`, `scos_forget` (memory) plus `scos_graphify_query`,
`scos_graphify_path`, `scos_graphify_explain` (graphify knowledge-graph
traversal) and `scos_code_repo_register`, `scos_code_repo_list`,
`scos_code_repo_get`, `scos_code_index`, `scos_code_query`,
`scos_code_impact` (code intelligence). A repository
must be registered with `scos_code_repo_register` before `scos_code_index`
will accept it.

## Install

From this repo (or any checkout that has `integrations/claude/`):

```bash
claude
/plugin marketplace add ./integrations/claude
/plugin install scos-memory-fabric@scos-fabric-tools
```

Claude Code prompts for four values on enable — see
`docs/agent-connection-guide.md` §2:

| Prompt | Source |
|---|---|
| SCOS gateway URL | defaults to the hosted deployment; override only if pointed at a different environment |
| SCOS API key | your own `scos_live_...` key from the SCOS portal (`/api/keys`), or `SCOS_BEARER_TOKEN` in `~/.hermes/scos.env` |
| Tenant ID | legacy/optional — leave blank, derived from the API key |
| Principal ID | legacy/optional — leave blank, derived from the API key |

The API key is marked `sensitive` — Claude Code stores it in your OS
keychain (or `~/.claude/.credentials.json` where no keychain is available),
never in plaintext `settings.json`, and never in this repo. Each key is
scoped to its own tenant workspace, so your memories are isolated from
every other user's key.

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
  of the ten tools (e.g. call `scos_recall` before answering from
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
every machine and minus the API key ever touching a config file that
could get synced or committed by mistake.
