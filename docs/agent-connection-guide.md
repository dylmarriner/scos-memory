# Connecting Agents to the SCOS Memory Fabric

This is the setup guide for pointing any MCP-capable coding agent at the
hosted SCOS stack (Docker Compose, on the fleet's Linode VPS). One shared
memory backbone, many clients — but **each user connects with their own
per-user API key**, so each person's memories live in their own isolated
tenant workspace, not a shared bucket.

## 1. How it fits together

```
 agent (Claude Code / Codex / Kilo / OpenCode / OpenClaw / Qwen / hermes-agent)
   │  stdio (JSON-RPC, MCP)
   ▼
 scos-mcp-bridge.py            ~/.hermes/scripts/scos-mcp-bridge.py
   │  HTTPS + Bearer <your scos_live_... API key>
   ▼
 https://scos.172.236.155.15.nip.io/mcp      (public, real TLS cert)
   │
   ▼
 cloud-control-plane (services/cloud-control-plane, :8079 internally)
   │  looks up your API key -> your tenant/principal, signs a short-lived
   │  internal claims token, proxies the call onward
   ▼
 mcp-gateway (services/mcp-gateway, internal-only, :8098)
   ├──► hermes-provider        (internal, :8088, TLS)  — memory tools
   │      └──► memory-gateway (:8080) ── PostgreSQL + pgvector
   ├──► memory-intelligence   (internal, :8089, TLS) — facts/correct/forget/dashboard
   ├──► graphify-provider     (internal, :8099, TLS) — knowledge-graph traversal
   └──► code-intelligence     (internal, :8084, TLS, control-plane)  — code tools
          └──► /v1/index, /v1/query, /v1/impact
```

Every agent talks to the **same** bridge script, which talks to the **same**
public gateway URL, so all agents you personally connect share your one
memory and code index — but nobody else's, since the gateway derives your
tenant/principal purely from your API key (`cloud-control-plane`'s
`/mcp` handler, `cmd/server/main.go`). The bridge exists because most agent
CLIs speak MCP over **stdio**, while the SCOS gateway speaks **HTTPS**. The
bridge translates one to the other and injects the auth header so individual
agent configs never need to embed the API key in plain JSON alongside
everything else.

`mcp-gateway` itself is **not** reachable directly from outside the VPS
(bound to `127.0.0.1` there on purpose) — `cloud-control-plane` is the only
sanctioned public entry point, because it's the layer that knows how to turn
a per-user API key into the right tenant. Don't try to bypass it by hitting
`mcp-gateway`'s port directly from a remote client; it won't be reachable,
and even from inside the VPS's own Docker network it only accepts requests
whose TLS hostname matches its certificate (see the SNI note in §5).

Tools exposed to every connected agent: `scos_recall`, `scos_status`,
`scos_explain`, `scos_user_model`, `scos_memory_graph`, `scos_correct`,
`scos_forget` (memory, via hermes-provider) plus `scos_graphify_query`,
`scos_graphify_path`, `scos_graphify_explain` (graphify knowledge-graph
traversal, via graphify-provider :8099), and `scos_code_repo_register`,
`scos_code_repo_list`, `scos_code_repo_get`, `scos_code_index`,
`scos_code_query`, `scos_code_impact` (code intelligence, via
code-intelligence :8084 — needs `SCOS_CONTROL_PLANE_TOKEN` in scos.env). A
repository must be registered with `scos_code_repo_register` (or `POST
/v1/repos` directly) before `scos_code_index` will accept files for it --
`scos_code_index` 404s against an unregistered `repository_id`.

## 2. One-time prerequisites (already done on this machine, listed for a fresh box)

1. **Get your own API key.** Sign up / log in at the SCOS portal
   (`cloud-control-plane`'s `/` and `/api/auth/signup` /
   `/api/auth/login`) and create a key under `/api/keys` (`name` is just a
   label, e.g. `"my-laptop"`). The response's `key` field
   (`scos_live_...`) is shown **once** — copy it immediately, it's stored
   server-side only as a hash. Each key is tied to your own tenant
   workspace: your `scos_recall`/`scos_status`/etc. calls only ever see
   memories your own key wrote.
2. **Verify the gateway is reachable** (public HTTPS, real cert — no VPN or
   Tailscale needed for this hop):
   ```bash
   curl -s https://scos.172.236.155.15.nip.io/mcp \
     -H "Authorization: Bearer <your scos_live_... key>" \
     -H "Content-Type: application/json" \
     -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"probe","version":"0.0.1"}}}'
   ```
   A working key returns a JSON-RPC `result` with `serverInfo.name ==
   "scos-mcp-gateway"`. `"API key required"` means the header is missing;
   `"invalid API key"` means it's wrong/revoked; `"gateway unavailable"`
   means the internal proxy hop from `cloud-control-plane` to `mcp-gateway`
   is broken (an infra problem, not a credentials problem — see §5).
3. **Shared bridge config** at `~/.hermes/scos.env` (mode `600`, never
   commit this — it holds your API key):
   ```bash
   SCOS_GATEWAY_URL=https://scos.172.236.155.15.nip.io/mcp
   SCOS_BEARER_TOKEN=<your scos_live_... API key>
   ```
   The other fields some older setups carry in this file
   (`SCOS_HERMES_BEARER_TOKEN`, `SCOS_TENANT_ID`, `SCOS_PRINCIPAL_ID`,
   `SCOS_TLS_CA_PATH`, ...) are legacy holdovers from a previous
   shared-identity/direct-to-`mcp-gateway` setup — harmless to leave in
   place, but only `SCOS_GATEWAY_URL` and `SCOS_BEARER_TOKEN` are read by
   the current bridge/gateway auth path. No CA cert file is needed anymore:
   the public gateway serves a real Let's Encrypt certificate, not a
   self-signed one.
4. **The bridge script** itself: `~/.hermes/scripts/scos-mcp-bridge.py`.

Steps 1–4 are per-machine (well, per-*person*, since your API key travels
with you), not per-agent — set them up once, then every agent below just
points at the same script with a different `SCOS_AGENT_NAME`.

The fastest way to do all of this at once, across every agent installed on
a machine, is `scripts/install_agent_plugins.py` — see §3a.

## 3a. Installing for every agent at once

`scripts/install_agent_plugins.py` wires up every MCP-capable agent
installed on the machine in one pass: it installs the shared bridge script
and `~/.hermes/plugins/scos-memory`, merges a `scos` MCP server entry into
each agent's native config (Cline, Cursor, Windsurf, Devin, Antigravity,
Qwen, OpenClaw, OpenCode, Continue, Kilo), links the Gemini extension, adds
the Codex plugin, and copies rules/slash-commands/skills where each agent
expects them. It's additive and takes a timestamped backup of every file it
touches (`<file>.bak-scos-<timestamp>`) before writing.

```bash
python3 scripts/install_agent_plugins.py
# if Gemini is already linked from a previous run:
python3 scripts/install_agent_plugins.py --skip-gemini
```

It does **not** write your API key anywhere — that still comes from
`~/.hermes/scos.env` (§2.3), which every agent's config reads indirectly
through the shared bridge script. Run this once per machine after setting
up `scos.env`; re-run it any time a new agent gets installed.

## 3. Per-agent config

Every entry below is identical except for `SCOS_AGENT_NAME`, which the
bridge uses to derive a stable session bucket
(`uuid5(NAMESPACE_DNS, "scos-mcp-{name}")`) — set a distinct name per agent
so recalls don't collide across tools.

### Claude Code

`~/.claude.json` → `mcpServers` (user scope, this machine only):
```json
"scos": {
  "type": "stdio",
  "command": "/usr/bin/python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "claude-code" }
}
```
*(Already configured on this machine.)*

To share this with teammates instead of configuring it per-machine, put the
same block in a project-scoped **`.mcp.json`** at the repo root (checked
into git) — Claude Code auto-loads it for anyone who opens this project,
CLI or Desktop. Don't put the bearer token in it; it isn't needed here since
the bridge reads the token from each machine's own `~/.hermes/scos.env`.

### mcp-gateway — native remote MCP (Claude Code `http` transport, Claude Desktop)

`hermes-provider` is scoped to the Hermes MemoryProvider lifecycle protocol
only (`services/hermes-provider/cmd/server/main.go` — `/v1/provider/*`); it
is not a general-purpose MCP surface and shouldn't be extended into one.
Instead there's a dedicated **`mcp-gateway`** service
(`services/mcp-gateway`) that speaks native MCP JSON-RPC over HTTPS and
forwards each governed tool call, with the caller's own identity headers, to
whichever service actually owns it:

| tool | forwarded to |
|---|---|
| `scos_status`, `scos_recall` | `hermes-provider` `/v1/provider/tool_call` |
| `scos_explain` | `memory-intelligence` `GET /v1/facts/{id}` |
| `scos_user_model` | `memory-intelligence` `GET /v1/dashboard/user-model` |
| `scos_memory_graph` | `memory-intelligence` `GET /v1/dashboard/memory-graph` |
| `scos_memory_summary` | `memory-intelligence` `GET /v1/dashboard/summary` — real LLM summary when `DEEPSEEK_API_KEY` is configured, otherwise a deterministic network-grouped digest (response's `mode` field says which) |
| `scos_graphify_query` | `graphify-provider` `POST /v1/graphify/query` |
| `scos_graphify_path` | `graphify-provider` `POST /v1/graphify/path` |
| `scos_graphify_explain` | `graphify-provider` `POST /v1/graphify/explain` |
| `scos_correct` | `memory-intelligence` `PATCH /v1/facts/{id}` — inserts a new fact with the corrected content and marks the old one `superseded` (facts are append-only; nothing is mutated in place) |
| `scos_forget` | `memory-intelligence` `DELETE /v1/facts/{id}[?cascade=true]` — soft-deletes the fact (`status='revoked'`); idempotent. `cascade=true` also revokes the fact's superseded predecessor chain |
| `scos_correct_batch` | `memory-intelligence` `POST /v1/facts/batch-correct` — up to 100 corrections in one call, each independent |
| `scos_forget_batch` | `memory-intelligence` `POST /v1/facts/batch-forget` — up to 100 forgets by ID in one call, each independent |
| `scos_fact_feedback` | `memory-intelligence` `POST /v1/facts/{id}/feedback` — records POSITIVE/NEGATIVE/VERY_NEGATIVE quality signal against a fact (stored only, no automatic action taken) |
| `scos_fact_history` | `memory-intelligence` `GET /v1/facts/{id}/history` — full append-only revision history for a fact, including versions from before a correction or after a forget |
| `scos_forget_all` | `memory-intelligence` `DELETE /v1/facts?confirm=true[&namespace_id=...]` — bulk soft-delete: one namespace, or (omitting `namespace_id`) every active fact for the tenant. Requires `confirm=true` |
| `scos_code_repo_register` | `code-intelligence` `POST /v1/repos` — register a repository (canonical path, remote identity, branch, language set) and get back the `repository_id` to index/query/impact against |
| `scos_code_repo_list` | `code-intelligence` `GET /v1/repos` — list repositories registered for the caller's tenant |
| `scos_code_repo_get` | `code-intelligence` `GET /v1/repos/{id}` — a registered repository's details, incl. its current index generation |
| `scos_code_index` | `code-intelligence` `POST /v1/index` — index (or re-index) files for a repository |
| `scos_code_query` | `code-intelligence` `POST /v1/query` — query the code index by symbol/path/intent |
| `scos_code_impact` | `code-intelligence` `POST /v1/impact` — change-impact set for modified symbols |

`mcp-gateway` itself (`compose-mcp-gateway-1` on the VPS) is internal-only —
it's bound to `127.0.0.1` on the host and only reachable from other
containers on the same Docker network, never directly from your laptop.
**Don't point any client at `mcp-gateway`'s port directly** (e.g. an old
`https://<host>:8098/mcp` URL); it will refuse the connection or, from
inside the VPS network, fail TLS verification (its certificate's SAN is the
public gateway hostname, not the internal Docker DNS name — `mcp-gateway`
is only reachable under the alias `scos.172.236.155.15.nip.io` that's been
added to its Docker network config for exactly this reason). The **only**
sanctioned entry point, for every client, is `cloud-control-plane`'s public
endpoint:

```
https://scos.172.236.155.15.nip.io/mcp
```

This is the **full** governed tool surface (136 tools incl. the code
indexer), unlike the bridge's 10. It serves a real Let's Encrypt
certificate, so no `--insecure`/SSL-verify-off flag is needed anywhere in
this section — that workaround only applied to the old self-signed setup.

**Claude Code** (CLI, and Desktop's embedded Claude Code sessions, which
share `~/.claude.json`):
```bash
claude mcp add --transport http --scope user scos-remote \
  https://scos.172.236.155.15.nip.io/mcp \
  -H "Authorization: Bearer <your scos_live_... API key>"
```
Verify with `claude mcp get scos-remote` — should show `✔ Connected`.
*(Already registered on this machine as `scos-remote`.)*

**Claude Desktop's account-level Connectors** (Settings → Connectors, the
same mechanism behind the `claude.ai *` remote tools — Notion, Vercel,
Zapier, etc.) can point at `https://scos.172.236.155.15.nip.io/mcp` too.
Whether that UI's "Add connector" flow accepts a static bearer header (vs.
requiring OAuth) hasn't been confirmed — if it doesn't, the Claude Code path
above already covers both CLI and Desktop's embedded sessions, which is the
same tool access either way.

### Codex

`~/.codex/config.toml`:
```toml
[mcp_servers.scos]
command = "/usr/bin/python3"
args = ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"]

[mcp_servers.scos.env]
SCOS_AGENT_NAME = "codex"
```
*(Already configured on this machine.)*

### Kilo Code

`~/.config/kilo/kilo.json` → `mcp` (note: array-form `command`, key is
`environment` not `env`):
```json
"mcp": {
  "scos": {
    "type": "local",
    "command": ["/usr/bin/python3", "-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
    "environment": { "SCOS_AGENT_NAME": "kilo" }
  }
}
```

### OpenCode

`~/.opencode/mcp.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "opencode" },
  "enabled": true
}
```

### OpenClaw

`~/.openclaw/mcp.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "openclaw" },
  "enabled": true
}
```

### Qwen Code CLI

`~/.qwen/settings.json` → `mcpServers`:
```json
"scos": {
  "command": "python3",
  "args": ["-E", "/home/kubuntux/.hermes/scripts/scos-mcp-bridge.py"],
  "env": { "SCOS_AGENT_NAME": "qwen" }
}
```

### hermes-agent

Despite what an earlier version of this doc said, hermes-agent does **not**
read a sibling file dropped under `~/.bsync_core/hermes-agent/` for this —
that directory holds its `brainsync` live-memory config, not general MCP
server registration. hermes-agent manages its own MCP servers via its `mcp`
subcommand, backed by `~/.hermes/config.yaml`:

```bash
hermes mcp add scos \
  --command /usr/bin/python3 \
  --args -E /home/kubuntux/.hermes/scripts/scos-mcp-bridge.py \
  --env SCOS_AGENT_NAME=hermes-agent
```

It connects immediately and does tool discovery live, so it'll ask whether
to enable all discovered tools (146 of them, the full gateway surface — say
yes, or pipe `yes |` in front of the command for a non-interactive install).
Verify any time with `hermes mcp list` (should show `scos` as `✓ enabled`)
or `hermes mcp test scos`.

## 4. Verifying a new connection

After wiring an agent, from that agent ask it to call `scos_status` — it
should return the provider's live status, not a transport error. To debug
the bridge directly without going through an agent:
```bash
SCOS_AGENT_NAME=debug SCOS_MCP_DEBUG=1 python3 ~/.hermes/scripts/scos-mcp-bridge.py <<'EOF'
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"scos_status","arguments":{}}}
EOF
```
`SCOS_MCP_DEBUG=1` echoes every message to stderr so you can see the raw
JSON-RPC traffic.

## 5. Troubleshooting

| Symptom | Likely cause |
|---|---|
| Agent hangs on startup / MCP timeout | Bridge is fine (`tools/list` answers from a static list instantly) — check the agent's own MCP startup timeout instead. |
| `transport error` in bridge stderr, or `curl` to the gateway just times out | Network reachability to `scos.172.236.155.15.nip.io` (public DNS/HTTPS, not VPN-gated) — check general internet connectivity first. |
| `"API key required"` | `SCOS_BEARER_TOKEN` missing/empty in `~/.hermes/scos.env`. |
| `"invalid API key"` | The key is wrong, or has been revoked — issue a fresh one via `/api/keys` and update `~/.hermes/scos.env`. |
| `"gateway unavailable"` (with a valid key) | Not a credentials problem — `cloud-control-plane`'s internal proxy hop to `mcp-gateway` is broken. Check `docker logs compose-cloud-control-plane-1` / `compose-mcp-gateway-1` on the VPS; the usual cause is `mcp-gateway`'s Docker network alias (`scos.172.236.155.15.nip.io`, baked into `deploy/compose/docker-compose.yml`) having been dropped by a manual `docker network disconnect`/container recreation outside `docker compose up -d` — reapply it or just re-run `docker compose up -d mcp-gateway` from the compose file. |
| HTTP 401/403 from code-intelligence on a `scos_code_*` call | `SCOS_CONTROL_PLANE_TOKEN` missing/mismatched in `~/.hermes/scos.env` — code tools need the control-plane token header in addition to the API key. |
| TLS handshake failure | The public gateway serves a real Let's Encrypt cert (`scos.172.236.155.15.nip.io`), so a handshake failure here means something is actually wrong (stale DNS, wrong port, a corporate TLS-inspecting proxy) — it's not the old self-signed-cert situation this doc used to describe. |
| `scos_recall` returns nothing / wrong bucket | Two agents sharing one `SCOS_AGENT_NAME` collide on the same session UUID — give each a unique name. |

## 6. Security notes

- `~/.hermes/scos.env` holds your API key in plaintext — keep it `chmod
  600`, never commit it, never echo it into a shared doc or chat. Anyone
  with the key gets full read/write access to your memory tenant, so treat
  it like a password — revoke and reissue via `/api/keys/revoke` +
  `/api/keys` if it ever leaks.
- All per-agent config snippets above reference the bridge script and env
  file by path; none of them embed the key directly, so agent config files
  (which are more likely to get synced/backed up/shared) stay secret-free.
- The gateway (`scos.172.236.155.15.nip.io`) terminates TLS with a real,
  publicly trusted certificate — no custom CA file or `--insecure` flag is
  needed anywhere in this doc anymore. Internal service-to-service hops
  (`cloud-control-plane` → `mcp-gateway` → `hermes-provider` /
  `memory-intelligence` / `graphify-provider` / `code-intelligence`) use a
  separate internal CA (`compose_tls-certs` Docker volume) that also now
  trusts the public gateway's Let's Encrypt root, so those hops verify
  correctly too.
