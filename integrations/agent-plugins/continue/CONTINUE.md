# SCOS Memory Fabric — Continue

Continue is model-agnostic — the model behind a given chat may change
between sessions (or even mid-project) based on `config.yaml`. Don't treat
fluency or tone differences across sessions as a sign memory is stale;
call `scos_recall` to check the actual stored context rather than
inferring from how a response "sounds" compared to before.

`scos_status`, `scos_explain`, `scos_user_model`, and `scos_memory_graph`
are read-only. `scos_correct` and `scos_forget` mutate memory regardless of
which model is currently configured — confirm the exact `memory_id` before
either, since a weaker model behind the chat is not a reason to skip that
confirmation.

This bridge is scoped to Continue's memory tier (recall/status/explain/
user_model/memory_graph/correct/forget) — code-intelligence and governance
tools are not exposed here.
