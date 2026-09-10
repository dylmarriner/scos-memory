import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const root = new URL("../", import.meta.url);

async function readJson(name) {
  return JSON.parse(await readFile(new URL(name, root), "utf8"));
}

test("manifest claims the memory slot and declares every runtime tool", async () => {
  const manifest = await readJson("openclaw.plugin.json");
  assert.equal(manifest.kind, "memory");
  assert.ok(manifest.categories.includes("memory"));
  assert.deepEqual(
    new Set(manifest.contracts.tools),
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
  assert.equal(manifest.toolMetadata.scos_remember.sideEffecting, true);
  assert.equal(manifest.toolMetadata.scos_forget.sideEffecting, true);
});

test("package pins a real OpenClaw SDK cohort rather than latest/placeholders", async () => {
  const pkg = await readJson("package.json");
  assert.equal(pkg.devDependencies.openclaw, "2026.9.3");
  assert.equal(pkg.openclaw.build.openclawVersion, "2026.9.3");
  assert.equal(pkg.openclaw.build.pluginSdkVersion, "2026.9.3");
  assert.match(pkg.openclaw.compat.pluginApi, /^>=2026\.9\.3$/);
  assert.ok(!JSON.stringify(pkg).includes("REPLACE_WITH"));
  assert.ok(!JSON.stringify(pkg).includes('"latest"'));
});
