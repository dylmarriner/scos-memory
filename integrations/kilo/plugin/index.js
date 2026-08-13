/** Kilo Code plugin: keeps SCOS memory usage explicit at session boundaries. */
export const ScosMemory = async (_ctx) => ({
  "chat.message": async (_input, output) => {
    output.parts = [
      ...(output.parts || []),
      {
        type: "text",
        text: "\n[SCOS] Use scos_recall for prior context. Confirm the exact memory_id before scos_correct or scos_forget.",
      },
    ];
  },
});

export default ScosMemory;
