import { deterministicMemoryKey, type ScosConfig } from "./core.js";

type JsonObject = Record<string, unknown>;

type RpcEnvelope = {
  result?: unknown;
  error?: { code?: number; message?: string };
};

function asRecord(value: unknown): JsonObject {
  return value && typeof value === "object" && !Array.isArray(value)
    ? (value as JsonObject)
    : {};
}

function unwrapMcpResult(value: unknown): unknown {
  const result = asRecord(value);
  const content = Array.isArray(result.content) ? result.content : [];
  for (const item of content) {
    const part = asRecord(item);
    if (part.type !== "text" || typeof part.text !== "string") continue;
    try {
      return JSON.parse(part.text) as unknown;
    } catch {
      return { text: part.text };
    }
  }
  return value;
}

export class ScosClient {
  private rpcId = 0;

  constructor(readonly config: ScosConfig) {}

  private headers(): Record<string, string> {
    return {
      "Content-Type": "application/json",
      "X-SCOS-Tenant-ID": this.config.tenantId,
      "X-SCOS-Principal-ID": this.config.principalId,
      "X-SCOS-Agent-Name": "openclaw",
      Authorization: `Bearer ${this.config.token}`,
    };
  }

  private async fetchJson(url: string, init: RequestInit): Promise<unknown> {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), this.config.requestTimeoutMs);
    try {
      const response = await fetch(url, { ...init, signal: controller.signal });
      const body = await response.text();
      if (!response.ok) {
        throw new Error(`SCOS HTTP ${response.status}: ${body.slice(0, 500)}`);
      }
      if (!body.trim()) return {};
      try {
        return JSON.parse(body) as unknown;
      } catch (error) {
        throw new Error(`SCOS returned invalid JSON from ${url}: ${String(error)}`);
      }
    } catch (error) {
      if (controller.signal.aborted) {
        throw new Error(`SCOS request timed out after ${this.config.requestTimeoutMs}ms`);
      }
      throw error;
    } finally {
      clearTimeout(timeout);
    }
  }

  async callGateway(name: string, args: JsonObject): Promise<unknown> {
    const payload = (await this.fetchJson(this.config.gatewayUrl, {
      method: "POST",
      headers: this.headers(),
      body: JSON.stringify({
        jsonrpc: "2.0",
        id: ++this.rpcId,
        method: "tools/call",
        params: { name, arguments: args },
      }),
    })) as RpcEnvelope;
    if (payload.error) {
      throw new Error(payload.error.message || `SCOS RPC error ${payload.error.code ?? "unknown"}`);
    }
    return unwrapMcpResult(payload.result);
  }

  async providerPost(path: string, payload: JsonObject): Promise<JsonObject> {
    const result = await this.fetchJson(`${this.config.providerUrl}${path}`, {
      method: "POST",
      headers: this.headers(),
      body: JSON.stringify({
        tenant_id: this.config.tenantId,
        principal_id: this.config.principalId,
        ...payload,
      }),
    });
    return asRecord(result);
  }

  async providerGet(path: string): Promise<JsonObject> {
    const result = await this.fetchJson(`${this.config.providerUrl}${path}`, {
      method: "GET",
      headers: this.headers(),
    });
    return asRecord(result);
  }

  async recall(query: string, maxItems: number, sessionId: string): Promise<JsonObject> {
    return asRecord(
      await this.callGateway("scos_recall", {
        query,
        max_items: maxItems,
        session_id: sessionId,
      }),
    );
  }

  async explain(memoryId: string): Promise<JsonObject> {
    return asRecord(await this.callGateway("scos_explain", { memory_id: memoryId }));
  }

  async forget(memoryId: string, cascade = false): Promise<JsonObject> {
    return asRecord(
      await this.callGateway("scos_forget", { memory_id: memoryId, cascade }),
    );
  }

  async status(): Promise<JsonObject> {
    return asRecord(await this.callGateway("scos_status", {}));
  }

  async remember(
    text: string,
    scope: "user" | "working",
    key?: string,
    agentId = "main",
  ): Promise<JsonObject> {
    const memoryKey = key?.trim() || deterministicMemoryKey(text, scope, agentId);
    return this.providerPost("/v1/provider/memory_write", {
      namespace_id: this.config.namespaceId,
      key: memoryKey,
      value: text,
      scope,
      source: "openclaw_memory:explicit",
      profile_path: "openclaw",
    });
  }

  async syncTurn(
    sessionId: string,
    turnId: string,
    messages: Array<{ role: "user" | "assistant"; content: string }>,
  ): Promise<JsonObject> {
    return this.providerPost("/v1/provider/sync_turn", {
      namespace_id: this.config.namespaceId,
      session_id: sessionId,
      turn_id: turnId,
      messages,
      profile_path: "openclaw",
    });
  }

  async endSession(sessionId: string): Promise<JsonObject> {
    return this.providerPost("/v1/provider/session_end", { session_id: sessionId });
  }
}
