import assert from "node:assert/strict";
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
