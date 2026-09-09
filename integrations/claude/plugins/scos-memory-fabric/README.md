# scos-memory-fabric (Claude Code plugin)

Wires Claude Code to a SCOS Memory Fabric `mcp-gateway` service over the
native `http` MCP transport. No default URL is baked in — point `gateway_url`
at any `mcp-gateway` deployment, self-hosted or otherwise. No local bridge
script; the gateway serves the full governed tool surface (136 tools incl.
the code indexer).

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

Preferred: run the shared installer first — it reads `~/.hermes/scos.env` and
pre-fills the plugin's config in `~/.claude/settings.json`, so you won't be
prompted for anything:

```bash
python3 scripts/install_agent_plugins.py
```

That script also runs `claude plugin marketplace add`/`install` for you if
the `claude` CLI is on your PATH. If you'd rather do it by hand (or the
script's `claude` step got skipped):

```bash
claude
/plugin marketplace add ./integrations/claude
/plugin install scos-memory-fabric@scos-fabric-tools
```

If nothing pre-filled the config, Claude Code prompts for four values on
enable — the same ones documented in `docs/agent-connection-guide.md` §2 (and
explained in plain language right there in each prompt), read from your own
`~/.hermes/scos.env`:

| Prompt | Source | What it actually is |
|---|---|---|
| mcp-gateway URL | `SCOS_GATEWAY_URL` in `~/.hermes/scos.env` | The address of your SCOS server |
| SCOS bearer token | `SCOS_BEARER_TOKEN` (falls back to `SCOS_HERMES_BEARER_TOKEN`) | A password proving you're allowed to connect |
| Tenant ID | `SCOS_TENANT_ID` in `~/.hermes/scos.env` | Which organization/workspace's memories you can see — isolates tenants from each other |
| Principal ID | `SCOS_PRINCIPAL_ID` in `~/.hermes/scos.env` | Who you are within that tenant, so memories can be attributed to a caller |

None of these are values you invent yourself — they come from whoever set up
your SCOS deployment (usually just your own `~/.hermes/scos.env` on a
single-user fleet). The bearer token is marked `sensitive` — Claude Code
stores it in your OS keychain (or `~/.claude/.credentials.json` where no
keychain is available), never in plaintext `settings.json`, and never in this
repo.

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
every machine and minus the bearer token ever touching a config file that
could get synced or committed by mistake.
