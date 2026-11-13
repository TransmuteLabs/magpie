import assert from "node:assert/strict";
import { test } from "node:test";
import { publicManifest, validateManifest } from "../index.js";
import { manifest } from "./helpers.js";
import { validateModule } from "../manifest.js";

function source() {
  return {
    ...manifest(["outbound-executor"]),
    contract: { version: { major: 1, minor: 3 }, capabilities: ["outbound-executor"] },
    providers: [{
      id: "vendor", name: "Vendor", protocols: ["chat"],
      operations: ["embeddings", "images.generate", "audio.speech", "videos.create", "videos.get", "videos.content"],
      models: [{
        id: "vector", kind: "embedding", inputModalities: ["text"], outputModalities: [],
        released: "2026-11-01", temperature: false, serviceTiers: ["priority"],
        contextWindow: 100, maxContextWindow: 200, rate: 0.5, rateWas: 1,
      }],
    }],
  };
}

test("provider operation and model policy retain explicit empty outputs and detach snapshots", () => {
  const original = source();
  const snapshot = publicManifest(original);
  original.providers[0].models[0].outputModalities.push("image");
  original.providers[0].operations[0] = "rerank";
  assert.deepEqual(snapshot.providers[0].models[0].outputModalities, []);
  assert.equal(snapshot.providers[0].models[0].temperature, false);
  assert.equal(snapshot.providers[0].operations[0], "embeddings");
});

test("provider operations require explicit negotiated authority", () => {
  for (const change of [
    value => { value.contract.version.minor = 2; },
    value => { value.providers[0].operations = ["fetch-anything"]; },
    value => { value.providers[0].operations = ["embeddings", "embeddings"]; },
    value => { value.providers[0].protocols = ["gemini"]; },
    value => { value.contract.capabilities = ["model-discovery"]; },
  ]) {
    const value = source();
    change(value);
    assert.throws(() => validateManifest(value));
  }
});

test("extended model policy rejects malformed metadata", () => {
  for (const patch of [
    { kind: "arbitrary" }, { inputModalities: ["secret"] }, { outputModalities: ["text", "text"] },
    { released: "2026-02-30" }, { serviceTiers: ["admin"] }, { serviceTiers: ["priority", "priority"] },
    { maxContextWindow: 99 }, { rate: -1 }, { temperature: "false" }, { defaultEffort: "high" },
  ]) {
    const value = source();
    Object.assign(value.providers[0].models[0], patch);
    assert.throws(() => validateManifest(value), JSON.stringify(patch));
  }
});

test("author provider handler never sees an undeclared operation", async () => {
  let calls = 0;
  const module = validateModule({
    manifest: source(),
    handlers: { "provider.execute": async () => { calls++; return { status: 200 }; } },
  });
  const handler = module.handlers.get("provider.execute");
  for (const params of [
    { protocol: "chat", operation: "images.edit" },
    { protocol: "gemini", operation: "embeddings" },
    { protocol: "chat", operation: "fetch-anything" },
  ]) {
    assert.throws(() => handler({}, { params, scope: { provider: "vendor" } }, {}));
  }
  assert.equal(calls, 0);
  await handler({}, { params: { protocol: "chat", operation: "embeddings" }, scope: { provider: "vendor" } }, {});
  assert.equal(calls, 1);
});
