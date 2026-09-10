# Hermes Agent integration

Two plugins live here. **Use `plugins/scos` — it supersedes `plugins/scos-memory`.**

Both share the same connection file, `~/.hermes/scos.env`, and default to real
TLS certificate verification. A misconfigured plugin raises clearly instead of
silently sending broken authentication or skipping certificate checks. For
local development only, set `SCOS_TLS_INSECURE_SKIP_VERIFY=1` explicitly; never
use that setting in production.

## `plugins/scos` — MemoryProvider (persistent memory, use this one)

`plugins/scos` is a first-class Hermes `MemoryProvider`, currently manifest
version **1.1.0**. It uses Hermes' documented `agent.memory_provider` interface
and participates in the automatic memory lifecycle, so persistence and recall
do not depend on the model remembering to call a memory tool.

### Lifecycle behavior

- **`prefetch()`** calls `scos_recall` on mcp-gateway before the model sees the
  prompt. Query-specific recall stays out of the stable system-provider block
  and is capped before injection.
- **`queue_prefetch()`** sends a session-scoped pre-warm request to
  `hermes-provider` after a turn. Hermes schedules this hook in its background
  provider executor.
- **`sync_turn()`** queues only the completed user/assistant exchange to a
  bounded FIFO worker and returns immediately. It does not re-submit the full
  transcript on every turn. Turn IDs are deterministic, and the backend's
  idempotency contract makes replay safe.
- **`on_pre_compress()`** implements Hermes checkpoint API v2. Hermes supplies
  normalized direct evidence; SCOS synchronously commits it through the
  idempotent `/v1/provider/sync_turn` path using a content-derived checkpoint
  ID before lossy compression is allowed to continue.
- **`on_memory_write()`** mirrors committed built-in `MEMORY.md` / `USER.md`
  writes into SCOS with source, scope, profile and deterministic provenance.
- **`on_session_end()`** drains pending turn writes, makes a final best-effort
  transcript checkpoint, then closes the session lifecycle in
  `hermes-provider`.
- **`get_tool_schemas()` / `handle_tool_call()`** continue exposing the dynamic
  SCOS MCP tool catalogue for explicit retrieval, inspection and correction.

The provider deliberately uses the existing SCOS lifecycle service instead of
reaching into Hermes' `state.db`. Hermes owns its local session database; SCOS
owns durable external memory authority in PostgreSQL.

### Fail-closed compression protection

The plugin advertises `pre_compress_checkpoint_api_version = 2`. To require a
durable SCOS checkpoint before Hermes performs a lossy context rewrite, enable
this in the active Hermes profile's `config.yaml`:

```yaml
compression:
  checkpoint_required: true
```

With that setting, an unavailable or failed SCOS checkpoint raises through the
Hermes v2 checkpoint contract and compression is blocked rather than silently
discarding unarchived direct evidence. Without it, checkpoint failure is
best-effort and logged while Hermes retains its normal compression behavior.

### Install and activate

```bash
cp -R integrations/hermes/plugins/scos ~/.hermes/plugins/scos
hermes memory setup
```

Pick `scos` when prompted. Hermes permits one external memory provider at a
time.

| Key | Required for | Purpose |
| --- | --- | --- |
| `SCOS_GATEWAY_URL` | recall + tools | mcp-gateway endpoint, saved in `~/.hermes/scos.json` |
| `SCOS_BEARER_TOKEN` | all SCOS calls | shared bearer token, stored through Hermes secret environment handling |
| `SCOS_TENANT_ID` / `SCOS_PRINCIPAL_ID` | recall + persistence | tenant/principal identity scoping, saved in `scos.json` |
| `SCOS_HERMES_PROVIDER_URL` | lifecycle persistence | SCOS `hermes-provider` base URL, saved in `scos.json` |
| `SCOS_NAMESPACE_ID` | persistence | namespace for Hermes turns and mirrored memory, saved in `scos.json` |
| `SCOS_TLS_CA_PATH` | optional TLS trust | custom CA bundle path when required |

`hermes memory setup` requires the complete configuration, stores non-secret
settings in the active profile's `scos.json`, stores the bearer token through
Hermes' secret environment handling, and checks both gateway recall and
`hermes-provider` health before reporting success.

`SCOS_HERMES_BEARER_TOKEN` remains supported as a legacy override when the
lifecycle service uses a distinct token. Normal deployments should use the
shared `SCOS_BEARER_TOKEN`.

### Profile and agent isolation

The plugin uses the `hermes_home` supplied by Hermes for all profile-local
paths. It does not hardcode `~/.hermes` after initialization. Non-primary agent
contexts do not emit ordinary turn or built-in-memory writes, preventing cron,
flush and delegated execution contexts from polluting the primary memory
stream.

### Provider independence

SCOS memory identity and persistence are independent of Hermes' model provider.
The Hermes model/inference settings (`OPENAI_*`, OpenRouter, DeepSeek, Ollama,
etc.) are not read by this integration. Changing inference providers therefore
leaves the SCOS tenant, principal, namespace, recall index and persisted turns
unchanged. `SCOS_GATEWAY_URL` and `SCOS_HERMES_PROVIDER_URL` name SCOS HTTP
services only; neither setting identifies an LLM provider.

### Validation

The provider contract tests are part of both the repository test target and CI:

```bash
python3 -m unittest discover -s integrations/hermes/plugins/scos -p 'test_*.py' -v
make test
```

The focused suite covers checkpoint API versioning, deterministic checkpoint
replay, required-checkpoint failure propagation, non-blocking ordered turn
writes, built-in memory mirroring, queued prefetch and session-end behavior.

## `plugins/scos-memory` — toolset plugin (superseded)

The original integration registers mcp-gateway tools generically through
`ctx.register_tool()` but has no automatic prefetch or persistence. The model
must choose to call `scos_recall` itself, which is exactly the lifecycle gap
`plugins/scos` closes.

It is kept for agents that intentionally want occasional explicit tool access
without adopting SCOS as the active memory provider. **Do not install both.**
Doing so double-registers the same SCOS tool names against the same gateway.

```bash
cp -R integrations/hermes/plugins/scos-memory ~/.hermes/plugins/scos-memory
```

No `/scos-recall`-style slash commands are registered here. Hermes memory
integration is provided through provider/tool registration rather than a
parallel command surface.
