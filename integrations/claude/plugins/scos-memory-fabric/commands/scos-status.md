---
description: Check SCOS Memory Fabric provider health and reachability
---

Call the `scos_status` MCP tool with no arguments and report its result
verbatim (or a short summary of it). If the call fails, show the error and
suggest checking `gateway_url`, `bearer_token`, `tenant_id`, and
`principal_id` in this plugin's config against `~/.hermes/scos.env`, per
`docs/agent-connection-guide.md`.
