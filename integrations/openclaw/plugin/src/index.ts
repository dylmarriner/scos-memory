import { Type } from "typebox"
import { defineToolPlugin } from "openclaw/plugin-sdk/tool-plugin"
import { existsSync, readFileSync } from "node:fs"
import { homedir } from "node:os"
import { join } from "node:path"
import { randomUUID } from "node:crypto"

/**
 * SCOS Memory Fabric <-> OpenClaw integration (v1.1.0).
 *
 * Two integration surfaces, both additive to whatever memory plugin owns the
 * `plugins.slots.memory` slot (currently `openclaw-honcho` on this fleet):
 *
 *   1. A non-exclusive memory corpus + prompt supplement, so OpenClaw's own
 *      `memory_search` / pre-turn memory prep also reaches SCOS. The memory
 *      slot itself stays owned by the active memory plugin; SCOS never tries
 *      to displace it.
 *   2. Native governed tools that call mcp-gateway directly (JSON-RPC
 *      `tools/call`), with retry, timeout, idempotent writes, and metrics.
 *
 * Credentials/identity come from, in precedence order:
 *   plugin config  >  process env  >  ~/.hermes/scos.env  >  defaults
 * This mirrors integrations/agent-plugins/scos-mcp-bridge.py.
 */

const ENV_PATH = join(homedir(), ".hermes", "scos.env")

const DEFAULTS: Record<string, string> = {
  SCOS_GATEWAY_URL: "https://127.0.0.1:8098/mcp",
}

type Env = Record<string, string>

function loadEnvFile(): Env {
  const env: Env = {}
  if (existsSync(ENV_PATH)) {
    for (const rawLine of readFileSync(ENV_PATH, "utf8").split("\n")) {
      const line = rawLine.trim()
      if (!line || line.startsWith("#") || !line.includes("=")) continue
      const idx = line.indexOf("=")
      env[line.slice(0, idx).trim()] = line.slice(idx + 1).trim()
    }
  }
  return env
}

/** Identity contract: config wins, then process env, then scos.env, then default. */
function resolveIdentity(config: ScosConfig): {
  gatewayUrl: string
  token: string
  tenantId: string
  principalId: string
  agentName: string
  caPath: string
  insecure: boolean
} {
  const file = loadEnvFile()
  const pick = (configValue: string | undefined, ...keys: string[]): string => {
    if (configValue && configValue.trim()) return configValue.trim()
    for (const key of keys) {
      const fromProc = process.env[key]
      if (fromProc && fromProc.trim()) return fromProc.trim()
      const fromFile = file[key]
      if (fromFile && fromFile.trim()) return fromFile.trim()
    }
    return ""
  }
  return {
    gatewayUrl:
      pick(config.gatewayUrl, "SCOS_GATEWAY_URL", "SCOS_MCP_GATEWAY_URL") ||
      DEFAULTS.SCOS_GATEWAY_URL,
    token: pick(config.bearerToken, "SCOS_BEARER_TOKEN", "SCOS_HERMES_BEARER_TOKEN"),
    tenantId: pick(config.tenantId, "SCOS_TENANT_ID"),
    principalId: pick(config.principalId, "SCOS_PRINCIPAL_ID"),
    agentName: pick(config.agentName, "SCOS_AGENT_NAME") || "openclaw",
    caPath: pick(config.tlsCaPath, "SCOS_TLS_CA_PATH"),
    insecure:
      (pick(undefined, "SCOS_TLS_INSECURE_SKIP_VERIFY") === "1") ||
      config.tlsInsecureSkipVerify === true,
  }
}

// ---------------------------------------------------------------------------
// Metrics + health (in-memory; reset on gateway restart)
// ---------------------------------------------------------------------------

type Metrics = {
  startedAt: number
  recalls: number
  writes: number
  errors: number
  retries: number
  lastRecallAt: number | null
  lastWriteAt: number | null
  lastError: string | null
  lastErrorAt: number | null
  consecutiveFailures: number
}

const metrics: Metrics = {
  startedAt: Date.now(),
  recalls: 0,
  writes: 0,
  errors: 0,
  retries: 0,
  lastRecallAt: null,
  lastWriteAt: null,
  lastError: null,
  lastErrorAt: null,
  consecutiveFailures: 0,
}

function noteError(err: unknown): void {
  metrics.errors += 1
  metrics.consecutiveFailures += 1
  metrics.lastError = formatError(err)
  metrics.lastErrorAt = Date.now()
}

function formatError(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

// ---------------------------------------------------------------------------
// mcp-gateway client: timeout, bounded retry, idempotency, graceful failure
// ---------------------------------------------------------------------------

let rpcId = 0

const TRANSIENT_STATUS = new Set([408, 425, 429, 500, 502, 503, 504])

function isTransient(err: unknown): boolean {
  if (err instanceof ScosHttpError) return TRANSIENT_STATUS.has(err.status)
  // Network level failures (fetch rejections, DNS, TLS, timeouts) are transient.
  return true
}

class ScosHttpError extends Error {
  constructor(
    readonly status: number,
    readonly body: string,
  ) {
    super(`mcp-gateway HTTP ${status}: ${body.slice(0, 300)}`)
    this.name = "ScosHttpError"
  }
}

class ScosConfigError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "ScosConfigError"
  }
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

type CallOptions = {
  timeoutMs?: number
  retries?: number
  idempotent?: boolean
  signal?: AbortSignal
}

async function callGateway(
  config: ScosConfig,
  name: string,
  args: Record<string, unknown>,
  opts: CallOptions = {},
): Promise<unknown> {
  const identity = resolveIdentity(config)
  // A local, no-auth gateway (SCOS_AUTH_MODE=local) can legitimately have no
  // token/tenant/principal. If some are set but not all, that is a config bug.
  const provided = [identity.token, identity.tenantId, identity.principalId].filter(Boolean).length
  if (provided !== 0 && provided !== 3) {
    throw new ScosConfigError(
      "SCOS identity is partial: set all of SCOS_BEARER_TOKEN, SCOS_TENANT_ID and " +
        "SCOS_PRINCIPAL_ID (or leave all three unset for a local no-auth gateway).",
    )
  }

  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    "X-SCOS-Agent-Name": identity.agentName,
  }
  if (provided === 3) {
    headers["X-SCOS-Tenant-ID"] = identity.tenantId
    headers["X-SCOS-Principal-ID"] = identity.principalId
    headers["Authorization"] = `Bearer ${identity.token}`
  }

  const payload = args && opts.idempotent ? { ...args, idempotency_key: randomUUID() } : args
  const body = JSON.stringify({
    jsonrpc: "2.0",
    id: ++rpcId,
    method: "tools/call",
    params: { name, arguments: payload },
  })

  const timeoutMs = opts.timeoutMs ?? config.requestTimeoutMs ?? 8000
  const maxRetries = opts.retries ?? config.maxRetries ?? 2
  let lastErr: unknown

  for (let attempt = 0; attempt <= maxRetries; attempt++) {
    if (attempt > 0) {
      metrics.retries += 1
      await sleep(Math.min(2000, 200 * 2 ** (attempt - 1)))
    }
    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), timeoutMs)
    const onAbort = (): void => controller.abort()
    opts.signal?.addEventListener("abort", onAbort)
    try {
      const response = await fetch(identity.gatewayUrl, {
        method: "POST",
        headers,
        body,
        signal: controller.signal,
      })
      if (!response.ok) {
        throw new ScosHttpError(response.status, await response.text())
      }
      const json = (await response.json()) as {
        result?: unknown
        error?: { message?: string; code?: number }
      }
      if (json.error) {
        // A JSON-RPC application error is a definitive answer, not transient.
        throw new Error(json.error.message || `SCOS error ${json.error.code ?? ""}`.trim())
      }
      return json.result
    } catch (err) {
      lastErr = err
      if (opts.signal?.aborted) throw err
      if (!isTransient(err)) break
    } finally {
      clearTimeout(timer)
      opts.signal?.removeEventListener("abort", onAbort)
    }
  }

  noteError(lastErr)
  throw lastErr
}

/** Unwrap the MCP `result.content[0].text` envelope into JSON when possible. */
function unwrap(result: unknown): unknown {
  if (result && typeof result === "object" && "content" in result) {
    const content = (result as { content?: Array<{ type?: string; text?: string }> }).content
    const text = content?.[0]?.text
    if (typeof text === "string") {
      try {
        return JSON.parse(text)
      } catch {
        return text
      }
    }
  }
  return result
}

type RecallItem = {
  memory_id?: string
  content?: string
  title?: string
  type?: string
  rank?: number
  confidence_score?: number
  created_at?: string
}

function recallItems(payload: unknown): RecallItem[] {
  if (payload && typeof payload === "object" && "recall" in payload) {
    const arr = (payload as { recall?: unknown }).recall
    if (Array.isArray(arr)) return arr as RecallItem[]
  }
  return []
}

async function recall(
  config: ScosConfig,
  query: string,
  maxResults: number,
  signal?: AbortSignal,
): Promise<RecallItem[]> {
  const result = unwrap(
    await callGateway(
      config,
      "scos_recall",
      { query, max_items: maxResults },
      { signal, timeoutMs: config.recallTimeoutMs ?? 6000, retries: 1 },
    ),
  )
  metrics.recalls += 1
  metrics.lastRecallAt = Date.now()
  metrics.consecutiveFailures = 0
  return recallItems(result)
}

// ---------------------------------------------------------------------------
// Config schema
// ---------------------------------------------------------------------------

const configSchema = Type.Object({
  gatewayUrl: Type.Optional(
    Type.String({ description: "SCOS mcp-gateway MCP endpoint. Falls back to SCOS_GATEWAY_URL." }),
  ),
  bearerToken: Type.Optional(
    Type.String({ description: "SCOS bearer token. Falls back to SCOS_BEARER_TOKEN in ~/.hermes/scos.env." }),
  ),
  tenantId: Type.Optional(Type.String({ description: "SCOS tenant id. Falls back to SCOS_TENANT_ID." })),
  principalId: Type.Optional(
    Type.String({ description: "SCOS principal id. Falls back to SCOS_PRINCIPAL_ID." }),
  ),
  agentName: Type.Optional(
    Type.String({ description: "X-SCOS-Agent-Name sent to mcp-gateway.", default: "openclaw" }),
  ),
  tlsCaPath: Type.Optional(Type.String({ description: "CA bundle for TLS verification." })),
  tlsInsecureSkipVerify: Type.Optional(
    Type.Boolean({ description: "Skip TLS verification (local dev only).", default: false }),
  ),
  requestTimeoutMs: Type.Optional(
    Type.Number({ description: "Per-attempt request timeout in ms.", default: 8000 }),
  ),
  recallTimeoutMs: Type.Optional(
    Type.Number({ description: "Timeout for recall/supplement calls in ms.", default: 6000 }),
  ),
  maxRetries: Type.Optional(
    Type.Number({ description: "Bounded retries for transient gateway failures.", default: 2 }),
  ),
  enableCorpusSupplement: Type.Optional(
    Type.Boolean({
      description: "Include SCOS in memory_search corpus results (additive; does not own the memory slot).",
      default: true,
    }),
  ),
  enablePromptSupplement: Type.Optional(
    Type.Boolean({ description: "Add a short SCOS guidance line to the memory prompt section.", default: true }),
  ),
  promptSummary: Type.Optional(
    Type.Boolean({
      description: "Inject a bounded cached SCOS fact summary pre-turn. Adds one cached gateway call.",
      default: false,
    }),
  ),
  promptSummaryTtlMs: Type.Optional(
    Type.Number({ description: "TTL for the cached pre-turn SCOS summary.", default: 300000 }),
  ),
  allowWrites: Type.Optional(
    Type.Boolean({
      description: "Expose scos_correct/scos_forget. Off by default; these mutate stored memory.",
      default: false,
    }),
  ),
})

type ScosConfig = {
  gatewayUrl?: string
  bearerToken?: string
  tenantId?: string
  principalId?: string
  agentName?: string
  tlsCaPath?: string
  tlsInsecureSkipVerify?: boolean
  requestTimeoutMs?: number
  recallTimeoutMs?: number
  maxRetries?: number
  enableCorpusSupplement?: boolean
  enablePromptSupplement?: boolean
  promptSummary?: boolean
  promptSummaryTtlMs?: number
  allowWrites?: boolean
}

// ---------------------------------------------------------------------------
// Pre-turn summary cache
// ---------------------------------------------------------------------------

let summaryCache: { at: number; lines: string[] } | null = null

async function cachedSummary(config: ScosConfig, signal?: AbortSignal): Promise<string[]> {
  const ttl = config.promptSummaryTtlMs ?? 300000
  if (summaryCache && Date.now() - summaryCache.at < ttl) return summaryCache.lines
  try {
    const result = unwrap(
      await callGateway(config, "scos_memory_summary", { max_facts: "40" }, { signal, retries: 0 }),
    )
    const lines = summaryLines(result)
    summaryCache = { at: Date.now(), lines }
    return lines
  } catch (err) {
    noteError(err)
    return [] // fail open: never block a turn on SCOS
  }
}

function summaryLines(payload: unknown): string[] {
  if (!payload) return []
  const text =
    typeof payload === "string"
      ? payload
      : typeof payload === "object" && "summary" in payload
        ? String((payload as { summary?: unknown }).summary ?? "")
        : JSON.stringify(payload)
  const cleaned = text.trim().slice(0, 1200)
  if (!cleaned) return []
  return ["## SCOS active context (cached)", cleaned]
}

// ---------------------------------------------------------------------------
// Plugin definition
// ---------------------------------------------------------------------------

const base = defineToolPlugin({
  id: "scos-memory",
  name: "SCOS Memory Fabric",
  description:
    "Additive SCOS memory corpus/prompt supplement plus governed read and (opt-in) write tools, called directly against mcp-gateway.",
  configSchema,
  tools: (tool) => [
    tool({
      name: "scos_memory_guidance",
      description: "Explain the SCOS memory workflow for the current agent task.",
      parameters: Type.Object({}),
      execute: async () => ({
        guidance:
          "SCOS is available additively: memory_search also reaches the SCOS corpus when the supplement is enabled. " +
          "Call scos_recall for query-specific prior context, including context from an earlier mode or session. " +
          "Confirm the exact memory_id before scos_correct or scos_forget.",
      }),
    }),
    tool({
      name: "scos_recall",
      description: "Search the SCOS Memory Fabric for context relevant to a query.",
      parameters: Type.Object({
        query: Type.String({ description: "Search query." }),
        maxResults: Type.Optional(Type.Number({ description: "Max results (default 6)." })),
      }),
      execute: async ({ query, maxResults }, config, context) => {
        const items = await recall(config, query, maxResults ?? 6, context.signal)
        return { query, count: items.length, recall: items }
      },
    }),
    tool({
      name: "scos_status",
      description: "Check SCOS Memory Fabric provider health and reachability.",
      parameters: Type.Object({}),
      execute: async (_params, config, context) =>
        unwrap(await callGateway(config, "scos_status", {}, { signal: context.signal, retries: 1 })),
    }),
    tool({
      name: "scos_health",
      description:
        "Local SCOS integration health: call counters, retries, errors, last successful recall/write.",
      parameters: Type.Object({}),
      execute: async (_params, config) => {
        const identity = resolveIdentity(config)
        return {
          ...metrics,
          uptimeMs: Date.now() - metrics.startedAt,
          configured: Boolean(identity.gatewayUrl),
          identityConfigured:
            [identity.token, identity.tenantId, identity.principalId].filter(Boolean).length === 3,
          gatewayUrl: identity.gatewayUrl,
          agentName: identity.agentName,
          corpusSupplement: config.enableCorpusSupplement ?? true,
          promptSupplement: config.enablePromptSupplement ?? true,
          writesEnabled: config.allowWrites ?? false,
        }
      },
    }),
    tool({
      name: "scos_explain",
      description: "Explain why a specific SCOS memory is held and what evidence supports it.",
      parameters: Type.Object({ memory_id: Type.String({ description: "Memory/fact id." }) }),
      execute: async ({ memory_id }, config, context) =>
        unwrap(await callGateway(config, "scos_explain", { memory_id }, { signal: context.signal })),
    }),
    tool({
      name: "scos_fact_history",
      description: "Full append-only revision history of a fact, including pre-correction/post-forget versions.",
      parameters: Type.Object({ memory_id: Type.String({ description: "Fact id." }) }),
      execute: async ({ memory_id }, config, context) =>
        unwrap(await callGateway(config, "scos_fact_history", { memory_id }, { signal: context.signal })),
    }),
    tool({
      name: "scos_memory_summary",
      description: "Summarize the caller's active SCOS facts (LLM summary or deterministic digest).",
      parameters: Type.Object({
        max_facts: Type.Optional(Type.String({ description: "Max facts (default 200, max 500)." })),
        network: Type.Optional(
          Type.String({ description: "world | experience | entity | belief | procedure | negative" }),
        ),
      }),
      execute: async (params, config, context) =>
        unwrap(await callGateway(config, "scos_memory_summary", params, { signal: context.signal })),
    }),
    tool({
      name: "scos_user_model",
      description: "Inspect the stored user representation in the SCOS fabric.",
      parameters: Type.Object({}),
      execute: async (_params, config, context) =>
        unwrap(await callGateway(config, "scos_user_model", {}, { signal: context.signal })),
    }),
    tool({
      name: "scos_memory_graph",
      description: "Inspect SCOS memory entities and their relationships.",
      parameters: Type.Object({}),
      execute: async (_params, config, context) =>
        unwrap(await callGateway(config, "scos_memory_graph", {}, { signal: context.signal })),
    }),
    tool({
      name: "scos_context_compile",
      description: "Compile raw input context into a bounded SCOS capsule for a target model/token budget.",
      parameters: Type.Object({
        input: Type.String({ description: "Raw input text." }),
        token_budget: Type.Optional(Type.Number({ description: "Token budget." })),
        model: Type.Optional(Type.String({ description: "Target model." })),
        profile: Type.Optional(Type.String({ description: "Compression profile." })),
        memory_references: Type.Optional(Type.Array(Type.String(), { description: "Referenced memory ids." })),
      }),
      execute: async (params, config, context) =>
        unwrap(await callGateway(config, "scos_context_compile", params, { signal: context.signal })),
    }),
    tool({
      name: "scos_fact_feedback",
      description:
        "Record quality feedback (POSITIVE|NEGATIVE|VERY_NEGATIVE) against a fact. Signal only; does not correct or forget it.",
      parameters: Type.Object({
        memory_id: Type.String({ description: "Fact id." }),
        feedback: Type.String({ description: "POSITIVE | NEGATIVE | VERY_NEGATIVE" }),
        reason: Type.Optional(Type.String({ description: "Why this feedback was given." })),
      }),
      execute: async (params, config, context) => {
        ensureWrites(config)
        const result = unwrap(
          await callGateway(config, "scos_fact_feedback", params, {
            signal: context.signal,
            idempotent: true,
          }),
        )
        metrics.writes += 1
        metrics.lastWriteAt = Date.now()
        return result
      },
    }),
    tool({
      name: "scos_correct",
      description:
        "Correct a stored memory by memory_id, preserving the superseded version. Requires exact memory_id and writes enabled.",
      parameters: Type.Object({
        memory_id: Type.String({ description: "Memory id to correct (must be exact)." }),
        value: Type.String({ description: "Corrected value." }),
        reason: Type.Optional(Type.String({ description: "Why this correction was made." })),
      }),
      execute: async (params, config, context) => {
        ensureWrites(config)
        const result = unwrap(
          await callGateway(config, "scos_correct", params, { signal: context.signal, idempotent: true }),
        )
        metrics.writes += 1
        metrics.lastWriteAt = Date.now()
        return result
      },
    }),
    tool({
      name: "scos_forget",
      description:
        "Request removal of a specific memory by memory_id. Requires exact memory_id and writes enabled.",
      parameters: Type.Object({
        memory_id: Type.String({ description: "Memory id to forget (must be exact)." }),
        cascade: Type.Optional(
          Type.Boolean({ description: "Also revoke this fact's superseded predecessor chain." }),
        ),
      }),
      execute: async (params, config, context) => {
        ensureWrites(config)
        const result = unwrap(
          await callGateway(config, "scos_forget", params, { signal: context.signal, idempotent: true }),
        )
        metrics.writes += 1
        metrics.lastWriteAt = Date.now()
        return result
      },
    }),
  ],
})

function ensureWrites(config: ScosConfig): void {
  if (!(config.allowWrites ?? false)) {
    throw new Error(
      "SCOS writes are disabled. Set plugins.entries.scos-memory.config.allowWrites=true to enable " +
        "scos_correct/scos_forget/scos_fact_feedback. Read tools and memory_search are unaffected.",
    )
  }
}

// ---------------------------------------------------------------------------
// Additive memory supplements (non-exclusive; the memory slot stays with the
// configured memory plugin, currently openclaw-honcho)
// ---------------------------------------------------------------------------

const SCOS_CORPUS = "scos"

const corpusSupplement = {
  async search(params: {
    query: string
    maxResults?: number
    agentId?: string
    agentSessionKey?: string
    sandboxed?: boolean
    signal?: AbortSignal
  }): Promise<
    Array<{
      corpus: string
      path: string
      title?: string
      kind?: string
      score: number
      snippet: string
      id?: string
      citation?: string
      provenanceLabel?: string
    }>
  > {
    const config = currentConfig()
    if (!(config.enableCorpusSupplement ?? true)) return []
    try {
      const items = await recall(config, params.query, params.maxResults ?? 8, params.signal)
      return items
        .filter((it) => typeof it.content === "string" && it.content.trim())
        .map((it) => ({
          corpus: SCOS_CORPUS,
          path: `scos://${it.memory_id ?? "unknown"}`,
          title: it.title || (it.content ?? "").slice(0, 120),
          kind: it.type ?? "fact",
          score: typeof it.rank === "number" ? it.rank : (it.confidence_score ?? 0.5),
          snippet: it.content ?? "",
          id: it.memory_id,
          citation: `scos:${it.memory_id ?? "unknown"}`,
          provenanceLabel: "SCOS Memory Fabric",
        }))
    } catch (err) {
      noteError(err)
      return [] // fail open; never break memory_search
    }
  },
  async get(params: {
    lookup: string
    fromLine?: number
    lineCount?: number
    agentId?: string
    agentSessionKey?: string
    sandboxed?: boolean
    signal?: AbortSignal
  }) {
    const config = currentConfig()
    try {
      const result = unwrap(
        await callGateway(config, "scos_explain", { memory_id: params.lookup }, { signal: params.signal }),
      )
      const content =
        typeof result === "string" ? result : JSON.stringify(result, null, 2)
      return {
        corpus: SCOS_CORPUS,
        path: `scos://${params.lookup}`,
        title: `SCOS memory ${params.lookup}`,
        kind: "fact",
        content,
        fromLine: 1,
        lineCount: content.split("\n").length,
        id: params.lookup,
        provenanceLabel: "SCOS Memory Fabric",
      }
    } catch (err) {
      noteError(err)
      return null
    }
  },
}

let activeConfig: ScosConfig = {}

function currentConfig(): ScosConfig {
  return activeConfig
}

// Wrap the tool-plugin entry so we keep its generated manifest metadata symbol
// while adding non-exclusive memory supplements at startup.
const originalRegister = base.register
base.register = (api: Parameters<typeof originalRegister>[0]) => {
  originalRegister(api)
  const startedConfig = readApiConfig(api)
  activeConfig = startedConfig

  if (startedConfig.enableCorpusSupplement ?? true) {
    api.registerMemoryCorpusSupplement(corpusSupplement)
  }

  if (startedConfig.enablePromptSupplement ?? true) {
    api.registerMemoryPromptSupplement(() => [
      "[SCOS] SCOS memory is available additively and is included in memory_search results. " +
        "Use scos_recall for query-specific prior context; confirm an exact memory_id before scos_correct or scos_forget.",
    ])
  }

  if (startedConfig.promptSummary ?? false) {
    api.registerMemoryPromptPreparation(async () => cachedSummary(startedConfig))
  }
}

/** Best-effort read of this plugin's resolved config from the plugin api. */
function readApiConfig(api: { config?: unknown; pluginConfig?: unknown }): ScosConfig {
  const direct = (api as { pluginConfig?: ScosConfig }).pluginConfig
  if (direct && typeof direct === "object") return direct
  const cfg = (api as { config?: { plugins?: { entries?: Record<string, { config?: ScosConfig }> } } }).config
  const entry = cfg?.plugins?.entries?.["scos-memory"]?.config
  return entry && typeof entry === "object" ? entry : {}
}

export default base

// Re-exported for tests.
export const __test__ = {
  resolveIdentity,
  recallItems,
  summaryLines,
  unwrap,
  metrics,
}
