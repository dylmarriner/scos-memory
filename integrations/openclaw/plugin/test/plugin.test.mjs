import assert from "node:assert/strict";
import http from "node:http";
import test from "node:test";

import plugin from "../dist/index.js";

test("plugin registers as a memory capability with Active Memory-compatible tools and lifecycle hooks", () => {
  const toolNames = [];
  const hookNames = [];
  let memoryCapability;
  let disabledService = false;

  plugin.register({
    id: "scos-memory",
    name: "SCOS Memory Fabric",
    version: "2.0.0",
    source: "test",
    registrationMode: "runtime",
    pluginConfig: {
      gatewayUrl: "http://127.0.0.1:18098/mcp",
      providerUrl: "http://127.0.0.1:18088",
      token: "test-token",
      tenantId: "tenant",
      principalId: "principal",
      namespaceId: "namespace",
      autoRecall: true,
      autoCapture: true,
    },
    registerMemoryCapability(capability) {
      memoryCapability = capability;
    },
    registerTool(_tool, options) {
      if (options?.name) toolNames.push(options.name);
    },
    on(name) {
      hookNames.push(name);
    },
    registerService() {
      disabledService = true;
    },
    logger: {
      debug() {},
      info() {},
      warn() {},
      error() {},
    },
  });

  assert.equal(disabledService, false);
  assert.equal(plugin.kind, "memory");
  assert.equal(typeof memoryCapability?.promptBuilder, "function");
  assert.deepEqual(
    new Set(toolNames),
    new Set([
      "memory_search",
      "memory_get",
      "scos_recall",
      "scos_remember",
      "scos_explain",
      "scos_forget",
      "scos_status",
      "scos_memory_guidance",
    ]),
  );
  assert.deepEqual(new Set(hookNames), new Set(["before_prompt_build", "agent_end", "session_end"]));

  const prompt = memoryCapability.promptBuilder({
    availableTools: new Set(["memory_search", "memory_get"]),
    citationsMode: "auto",
  });
  assert.ok(prompt.some((line) => line.includes("authoritative persistent memory")));
  assert.ok(prompt.some((line) => line.includes("historical evidence")));
});

test("plugin fails closed instead of registering a half-configured memory provider", () => {
  let capabilityRegistered = false;
  let service;
  const warnings = [];
  plugin.register({
    id: "scos-memory",
    name: "SCOS Memory Fabric",
    source: "test",
    registrationMode: "runtime",
    pluginConfig: { tenantId: "tenant-only" },
    registerMemoryCapability() {
      capabilityRegistered = true;
    },
    registerTool() {},
    on() {},
    registerService(value) {
      service = value;
    },
    logger: {
      debug() {},
      info() {},
      warn(message) { warnings.push(message); },
      error() {},
    },
  });

  assert.equal(capabilityRegistered, false);
  assert.equal(service?.id, "scos-memory");
  service.start();
  assert.ok(warnings.some((message) => /disabled until configured/i.test(message)));
});

test("memory_search applies minScore to the recalled memories", async (t) => {
  const server = http.createServer(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
    const recall = {
      memories: [
        { memory_id: "m-high", content: "deploy window is Tuesday", fused_score: 0.42, rank: 1 },
        { memory_id: "m-low", content: "unrelated note", fused_score: 0.01, rank: 2 },
      ],
    };
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({
      jsonrpc: "2.0",
      id: body.id,
      result: { content: [{ type: "text", text: JSON.stringify(recall) }] },
    }));
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => server.close());
  const { port } = server.address();

  const factories = new Map();
  plugin.register({
    id: "scos-memory",
    name: "SCOS Memory Fabric",
    version: "2.0.0",
    source: "test",
    registrationMode: "runtime",
    pluginConfig: {
      gatewayUrl: `http://127.0.0.1:${port}/mcp`,
      providerUrl: `http://127.0.0.1:${port}`,
      token: "test-token",
      tenantId: "tenant",
      principalId: "principal",
      namespaceId: "namespace",
    },
    registerMemoryCapability() {},
    registerTool(factory, options) {
      if (options?.name) factories.set(options.name, factory);
    },
    on() {},
    registerService() {},
    logger: { debug() {}, info() {}, warn() {}, error() {} },
  });

  const tool = factories.get("memory_search")({ agentId: "main", sessionKey: "s" });
  const filtered = await tool.execute("call-1", { query: "deploy", minScore: 0.1 });
  assert.deepEqual(filtered.details.memories.map((m) => m.memoryId), ["m-high"]);
  assert.equal(filtered.details.count, 1);
  assert.equal(filtered.details.memories[0].score, 0.42);

  const unfiltered = await tool.execute("call-2", { query: "deploy" });
  assert.deepEqual(unfiltered.details.memories.map((m) => m.memoryId), ["m-high", "m-low"]);
});
