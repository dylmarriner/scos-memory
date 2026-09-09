# scos-memory-fabric-cowork (Claude Cowork plugin)

Connects Claude Cowork to a SCOS Memory Fabric `mcp-gateway` service over the
native `http` MCP transport, so Claude can recall, inspect, and correct the
user's persistent cross-session memory during Cowork sessions.

No default URL is baked in — this plugin works against any `mcp-gateway`
deployment, self-hosted or otherwise. You supply the connection details on
install.

Sibling to [`../scos-memory-fabric/`](../scos-memory-fabric/), the Claude
Code version of this plugin — same protocol, same seven memory tools
(`scos_recall`, `scos_status`, `scos_explain`, `scos_user_model`,
`scos_memory_graph`, `scos_correct`, `scos_forget`). This one drops the
slash commands and `SessionStart` hook the Code plugin has (Cowork favors a
single skill over both) and doesn't reach for the broader code-intelligence
/ graphify tool surface, since Cowork use is knowledge work, not a codebase.

## Install

```bash
# In Claude Cowork
Add plugin from: ./integrations/claude
Install: scos-memory-fabric-cowork
```

You'll be prompted for four values on enable:

| Prompt | What it actually is |
|---|---|
| mcp-gateway URL | The address of your SCOS server |
| SCOS bearer token | A password proving you're allowed to connect |
| Tenant ID | Which organization/workspace's memories you can see |
| Principal ID | Who you are within that tenant |

None of these are values you invent yourself — get them from whoever runs
your SCOS Memory Fabric deployment. The bearer token is marked `sensitive`
and stored in your OS keychain, never in plaintext config or this repo.

## What the plugin adds

- **Skill** (`skills/scos-memory/`): teaches Claude when to reach for each of
  the seven memory tools, to call `scos_recall` proactively before answering
  from assumption, and to always confirm the exact `memory_id` with the user
  before `scos_correct` or `scos_forget` — both mutate memory, and `forget`
  isn't easily undone.
