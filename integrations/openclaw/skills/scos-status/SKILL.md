---
name: scos-status
description: Check SCOS Memory Fabric provider health and reachability
user-invocable: true
---

Call the `scos_status` MCP tool with no arguments and report its result
verbatim (or a short summary of it). If the call fails, show the error and
suggest checking `~/.hermes/scos.env`, per `docs/agent-connection-guide.md`.
