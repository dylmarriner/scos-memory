import type { Plugin } from "@opencode-ai/plugin";

// Reminds the agent, once per session, to use the `scos` MCP tools
// (registered separately via mcp.json) instead of assuming context it
// doesn't have. Mirrors the SessionStart hook shipped with the Claude
// Code and Codex packagings of this same integration — see
// integrations/claude/plugins/scos-memory-fabric/hooks/hooks.json and
// integrations/codex/plugins/scos-memory-fabric/hooks.json.
const REMINDER =
  "SCOS Memory Fabric is connected (scos_recall, scos_status, scos_explain, " +
  "scos_user_model, scos_memory_graph, scos_correct, scos_forget). Before " +
  "answering questions that depend on prior sessions or stated preferences, " +
  "call scos_recall first instead of assuming. scos_correct and scos_forget " +
  "mutate stored memory - always confirm the exact memory_id with the user " +
  "before attempting either.";

const remindedSessions = new Set<string>();

export const ScosMemoryReminder: Plugin = async () => {
  return {
    event: async ({ event }) => {
      if (event.type === "session.created") {
        const sessionID = (event as any).properties?.info?.id as string | undefined;
        if (sessionID) remindedSessions.delete(sessionID);
      }
    },

    "experimental.chat.system.transform": async (input, output) => {
      const sessionID = input.sessionID;
      if (!sessionID || remindedSessions.has(sessionID)) return;
      remindedSessions.add(sessionID);
      output.system.push(REMINDER);
    },
  };
};
