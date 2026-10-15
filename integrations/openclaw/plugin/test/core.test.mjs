import assert from "node:assert/strict";
import http from "node:http";
import test from "node:test";

import {
  deriveDeterministicUuid,
  extractLastCompletedTurn,
  filterByMinScore,
  formatRecallContext,
  normalizeRecallResult,
  resolveScosConfig,
} from "../dist/core.js";
import { ScosClient } from "../dist/scos-client.js";

test("resolveScosConfig prefers plugin config and falls back to SCOS environment", () => {
  const config = resolveScosConfig(
    {
      gatewayUrl: "http://plugin-gateway/mcp",
      tenantId: "tenant-plugin",
      autoRecall: false,
      recallLimit: 6,
    },
    {
      SCOS_GATEWAY_URL: "http://env-gateway/mcp",
      SCOS_MEMORY_PROVIDER_URL: "http://provider",
      SCOS_BEARER_TOKEN: "secret",
      SCOS_TENANT_ID: "tenant-env",
      SCOS_PRINCIPAL_ID: "principal",
      SCOS_NAMESPACE_ID: "namespace",
    },
  );

  assert.equal(config.gatewayUrl, "http://plugin-gateway/mcp");
  assert.equal(config.providerUrl, "http://provider");
  assert.equal(config.token, "secret");
  assert.equal(config.tenantId, "tenant-plugin");
  assert.equal(config.principalId, "principal");
  assert.equal(config.namespaceId, "namespace");
  assert.equal(config.autoRecall, false);
  assert.equal(config.autoCapture, true);
  assert.equal(config.recallLimit, 6);
});

test("resolveScosConfig fails closed when identity or token is absent", () => {
  assert.throws(
    () => resolveScosConfig({}, { SCOS_TENANT_ID: "tenant" }),
    /token.*principal.*namespace/i,
  );
});

test("deriveDeterministicUuid is stable, distinct, and UUID shaped", () => {
  const a = deriveDeterministicUuid("agent/session/a");
  const b = deriveDeterministicUuid("agent/session/a");
  const c = deriveDeterministicUuid("agent/session/b");
  assert.equal(a, b);
  assert.notEqual(a, c);
  assert.match(a, /^[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

test("extractLastCompletedTurn captures only the most recent user/assistant exchange", () => {
  const turn = extractLastCompletedTurn([
    { role: "user", content: "older question" },
    { role: "assistant", content: "older answer" },
    { role: "user", content: [{ type: "text", text: "latest question" }] },
    { role: "assistant", content: [{ type: "text", text: "latest answer" }] },
  ]);
  assert.deepEqual(turn, { userContent: "latest question", assistantContent: "latest answer" });
});

test("formatRecallContext bounds injected memory and labels it untrusted historical data", () => {
  const text = formatRecallContext(
    { recall: [{ memory_id: "m1", content: "A".repeat(300) }] },
    180,
  );
  assert.match(text, /untrusted historical data/i);
  assert.ok(text.length <= 180);
});

test("filterByMinScore drops memories below the threshold and unscored memories", () => {
  const memories = normalizeRecallResult({
    memories: [
      { memory_id: "strong", content: "a", fused_score: 0.9, rank: 1 },
      { memory_id: "edge", content: "b", fused_score: 0.5, rank: 2 },
      { memory_id: "weak", content: "c", fused_score: 0.1, rank: 3 },
      { memory_id: "unscored", content: "d", rank: 4 },
    ],
  });
  assert.equal(memories[0].score, 0.9);
  assert.equal(memories[3].score, null);
  assert.deepEqual(filterByMinScore(memories, 0.5).map((m) => m.memoryId), ["strong", "edge"]);
  assert.deepEqual(
    filterByMinScore(memories, undefined).map((m) => m.memoryId),
    ["strong", "edge", "weak", "unscored"],
  );
  assert.deepEqual(filterByMinScore(memories, 2), []);
});

test("ScosClient sends authenticated gateway calls and unwraps MCP text JSON", async (t) => {
  let seenHeaders;
  let seenBody;
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    seenHeaders = req.headers;
    seenBody = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({
      jsonrpc: "2.0",
      id: seenBody.id,
      result: { content: [{ type: "text", text: JSON.stringify({ recall: [{ memory_id: "m1", content: "remembered" }] }) }] },
    }));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const { port } = server.address();

  const client = new ScosClient({
    gatewayUrl: `http://127.0.0.1:${port}/mcp`,
    providerUrl: `http://127.0.0.1:${port}`,
    token: "token-1",
    tenantId: "tenant-1",
    principalId: "principal-1",
    namespaceId: "namespace-1",
    autoRecall: true,
    autoCapture: true,
    recallLimit: 8,
    recallMaxChars: 4000,
    requestTimeoutMs: 2000,
    publicArtifactsDir: "",
  });

  const result = await client.recall("where is it?", 5, "session-uuid");
  assert.equal(seenHeaders.authorization, "Bearer token-1");
  assert.equal(seenHeaders["x-scos-tenant-id"], "tenant-1");
  assert.equal(seenHeaders["x-scos-principal-id"], "principal-1");
  assert.equal(seenHeaders["x-scos-agent-name"], "openclaw");
  assert.equal(seenBody.method, "tools/call");
  assert.equal(seenBody.params.name, "scos_recall");
  assert.deepEqual(seenBody.params.arguments, {
    query: "where is it?",
    max_items: 5,
    session_id: "session-uuid",
  });
  assert.equal(result.recall[0].memory_id, "m1");
});

test("ScosClient remember uses durable provider memory_write contract", async (t) => {
  let seenBody;
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    seenBody = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    assert.equal(req.url, "/v1/provider/memory_write");
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ mirrored: true, memory_id: "m1", version_number: 1 }));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const { port } = server.address();
  const client = new ScosClient({
    gatewayUrl: `http://127.0.0.1:${port}/mcp`, providerUrl: `http://127.0.0.1:${port}`,
    token: "t", tenantId: "tenant", principalId: "principal", namespaceId: "ns",
    autoRecall: true, autoCapture: true, recallLimit: 8, recallMaxChars: 4000,
    requestTimeoutMs: 2000, publicArtifactsDir: "",
  });

  const result = await client.remember("keep this", "user", "openclaw:test-key");
  assert.equal(seenBody.namespace_id, "ns");
  assert.equal(seenBody.key, "openclaw:test-key");
  assert.equal(seenBody.value, "keep this");
  assert.equal(seenBody.scope, "user");
  assert.equal(seenBody.source, "openclaw_memory:explicit");
  assert.equal(result.memory_id, "m1");
});
