import type { Plugin } from "@opencode-ai/plugin"

/** OpenCode-native SCOS reminder hook; tools are supplied by the companion MCP entry. */
export const ScosMemory: Plugin = async () => ({
  "chat.message": async (_input, output) => {
    output.parts = [
      ...(output.parts || []),
      {
        type: "text",
        text: "[SCOS] Recall prior context with scos_recall; confirm exact memory_id before correction or forgetting.",
      },
    ]
  },
})

export default ScosMemory
