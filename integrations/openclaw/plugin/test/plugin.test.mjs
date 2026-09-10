// Dependency-free tests for the SCOS OpenClaw plugin surface.
// Run with:  node --test test/
import test from "node:test"
import assert from "node:assert/strict"

import entry, { __test__ } from "../dist/index.js"
import { getToolPluginMetadata } from "openclaw/plugin-sdk/tool-plugin"

const metadata = getToolPluginMetadata(entry)
const toolNames = (metadata?.tools ?? []).map((t) => t.name)

test("entry identity matches the manifest", () => {
  assert.equal(entry.id, "scos-memory")
  assert.equal(entry.name, "SCOS Memory Fabric")
})

test("declares the full read + governed write tool surface", () => {
  const expected = [
    "scos_memory_guidance",
    "scos_recall",
    "scos_status",
    "scos_health",
    "scos_explain",
    "scos_fact_history",
    "scos_memory_summary",
    "scos_user_model",
    "scos_memory_graph",
    "scos_context_compile",
    "scos_fact_feedback",
    "scos_correct",
    "scos_forget",
  ]
  for (const name of expected) {
    assert.ok(toolNames.includes(name), `missing tool ${name}`)
  }
})

test("writes stay off by default in the config schema", () => {
  const schema = metadata?.configSchema
  assert.equal(schema?.properties?.allowWrites?.default, false)
  assert.equal(schema?.properties?.enableCorpusSupplement?.default, true)
})

test("register is composed (extends the tool-only base with supplements)", () => {
  assert.equal(typeof entry.register, "function")
})

test("unwrap parses the MCP content envelope", () => {
  const wrapped = { content: [{ type: "text", text: '{"recall":[]}' }] }
  assert.deepEqual(__test__.unwrap(wrapped), { recall: [] })
  assert.equal(__test__.unwrap({ content: [{ text: "plain" }] }), "plain")
  assert.equal(__test__.unwrap("raw"), "raw")
})

test("recallItems tolerates missing/shape-variant payloads", () => {
  assert.deepEqual(__test__.recallItems({ recall: [{ memory_id: "a", content: "b" }] }), [
    { memory_id: "a", content: "b" },
  ])
  assert.deepEqual(__test__.recallItems({}), [])
  assert.deepEqual(__test__.recallItems(null), [])
})

test("summaryLines bounds and prefixes the digest", () => {
  assert.deepEqual(__test__.summaryLines(""), [])
  const lines = __test__.summaryLines({ summary: "hello" })
  assert.equal(lines.length, 2)
  assert.match(lines[0], /SCOS active context/)
  assert.equal(lines[1], "hello")
})

test("resolveIdentity prefers explicit config over env", () => {
  const id = __test__.resolveIdentity({
    gatewayUrl: "https://example.invalid/mcp",
    tenantId: "t1",
    principalId: "p1",
    bearerToken: "tok",
    agentName: "openclaw-test",
  })
  assert.equal(id.gatewayUrl, "https://example.invalid/mcp")
  assert.equal(id.tenantId, "t1")
  assert.equal(id.principalId, "p1")
  assert.equal(id.agentName, "openclaw-test")
})
