import { Type } from "typebox"
import { defineToolPlugin } from "openclaw/plugin-sdk/tool-plugin"

export default defineToolPlugin({
  id: "scos-memory",
  name: "SCOS Memory Fabric",
  description: "SCOS memory workflow guidance; governed tools are exposed by the companion MCP server.",
  tools: (tool) => [tool({
    name: "scos_memory_guidance",
    description: "Explain the SCOS memory workflow for the current agent task.",
    parameters: Type.Object({}),
    execute: async () => ({
      guidance: "Call scos_recall for prior context. Confirm the exact memory_id before scos_correct or scos_forget.",
    }),
  })],
})
