import { Type } from "typebox"
import { defineToolPlugin } from "openclaw/plugin-sdk/tool-plugin"
import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"

// Mirrors the config loading in integrations/agent-plugins/scos-mcp-bridge.py
// so both paths into mcp-gateway agree on where credentials live.
const ENV_PATH = join(homedir(), ".hermes", "scos.env")

const DEFAULTS: Record<string, string> = {
  SCOS_GATEWAY_URL: "https://127.0.0.1:8098/mcp",
}

function loadEnv(): Record<string, string> {
  const env = { ...DEFAULTS }
  if (existsSync(ENV_PATH)) {
    for (const rawLine of readFileSync(ENV_PATH, "utf8").split("\n")) {
      const line = rawLine.trim()
      if (!line || line.startsWith("#") || !line.includes("=")) continue
      const idx = line.indexOf("=")
      env[line.slice(0, idx)] = line.slice(idx + 1)
    }
  }
  for (const key of Object.keys(env)) {
    const override = process.env[key]
    if (override) env[key] = override
  }
  return env
}

let rpcId = 0

async function callGateway(name: string, args: Record<string, unknown>): Promise<unknown> {
  const env = loadEnv()
  const token = env.SCOS_BEARER_TOKEN || env.SCOS_HERMES_BEARER_TOKEN || ""
  const tenantId = env.SCOS_TENANT_ID || ""
  const principalId = env.SCOS_PRINCIPAL_ID || ""
  if (!token || !tenantId || !principalId) {
    throw new Error(
      "SCOS_BEARER_TOKEN, SCOS_TENANT_ID and SCOS_PRINCIPAL_ID are required in ~/.hermes/scos.env",
    )
  }

  const response = await fetch(env.SCOS_GATEWAY_URL, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-SCOS-Tenant-ID": tenantId,
      "X-SCOS-Principal-ID": principalId,
      "X-SCOS-Agent-Name": "openclaw",
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({
      jsonrpc: "2.0",
      id: ++rpcId,
      method: "tools/call",
      params: { name, arguments: args },
    }),
  })

  if (!response.ok) {
    const body = await response.text()
    throw new Error(`mcp-gateway HTTP ${response.status}: ${body.slice(0, 500)}`)
  }

  const payload = (await response.json()) as { result?: unknown; error?: { message: string } }
  if (payload.error) throw new Error(payload.error.message)
  return payload.result
}

export default defineToolPlugin({
  id: "scos-memory",
  name: "SCOS Memory Fabric",
  description:
    "SCOS memory workflow guidance plus read-only governed tools, called directly against mcp-gateway.",
  tools: (tool) => [
    tool({
      name: "scos_memory_guidance",
      description: "Explain the SCOS memory workflow for the current agent task.",
      parameters: Type.Object({}),
      execute: async () => ({
        guidance:
          "Call scos_recall for prior context. Confirm the exact memory_id before scos_correct or scos_forget.",
      }),
    }),
    tool({
      name: "scos_recall",
      description: "Search the SCOS Memory Fabric for context relevant to a query.",
      parameters: Type.Object({ query: Type.String() }),
      execute: async ({ query }) => callGateway("scos_recall", { query }),
    }),
    tool({
      name: "scos_status",
      description: "Check SCOS Memory Fabric provider health and reachability.",
      parameters: Type.Object({}),
      execute: async () => callGateway("scos_status", {}),
    }),
    tool({
      name: "scos_explain",
      description: "Explain why a specific SCOS memory is held and what evidence supports it.",
      parameters: Type.Object({ memory_id: Type.String() }),
      execute: async ({ memory_id }) => callGateway("scos_explain", { memory_id }),
    }),
  ],
})
