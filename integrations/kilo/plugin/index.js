/**
 * Kilo Code plugin: keeps SCOS memory usage explicit across mode switches.
 *
 * Kilo's modes (Architect, Code, Ask, Debug) are separate contexts a task
 * can be handed between, not separate sessions with their own memory — a
 * plan made in Architect mode and a fix made in Debug mode are the same
 * SCOS session. The reminder fires on every chat message (Kilo's plugin API
 * has no session-boundary hook to fire it once), so it's kept to one line
 * rather than the fuller guidance the skill-capable bundles carry.
 */
export const ScosMemory = async (_ctx) => ({
  "chat.message": async (_input, output) => {
    output.parts = [
      ...(output.parts || []),
      {
        type: "text",
        text: "\n[SCOS] Use scos_recall for prior context, including context from a different mode earlier in this task. Confirm the exact memory_id before scos_correct or scos_forget.",
      },
    ];
  },
});

export default ScosMemory;
