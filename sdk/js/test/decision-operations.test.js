import assert from "node:assert/strict";
import { test } from "node:test";
import { publicManifest, validateManifest } from "../index.js";
import { validateModule } from "../manifest.js";

function decisionManifest() {
  return {
    schema: 1, id: "fixture.decision", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 7 }, capabilities: ["outbound-executor", "model-discovery"] },
    providers: [{
      id: "judge", name: "Judge", protocols: ["chat"], operations: ["systemone"],
      models: [
        { id: "reasoner", kind: "chat", protocols: ["chat"] },
        { id: "decision", kind: "decision", protocols: [], operations: ["systemone"] },
      ],
    }],
  };
}

test("SDK 1.7 host compatibility retains its negotiated floor", () => {
  const manifest = {
    schema: 1, id: "fixture.compatibility", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 7 }, capabilities: ["operation-control"] },
    compatibility: { hostAPI: "^1.7" },
  };
  assert.doesNotThrow(() => validateManifest(manifest));
  manifest.contract.version.minor = 6;
  assert.throws(() => validateManifest(manifest));
  delete manifest.compatibility;
  assert.doesNotThrow(() => validateManifest(manifest));
  manifest.contract.version.minor = 7;
  manifest.compatibility = { hostAPI: "^1.8" };
  assert.throws(() => validateManifest(manifest));
});

test("SDK 1.7 decision models retain isolated operation authority", () => {
  const value = decisionManifest();
  const snapshot = publicManifest(value);
  value.providers[0].models[1].operations[0] = "embeddings";
  assert.deepEqual(snapshot.providers[0].models[1].operations, ["systemone"]);
  assert.deepEqual(snapshot.providers[0].models[1].protocols, []);
  const isolated = decisionManifest();
  isolated.providers[0].protocols = [];
  isolated.providers[0].models.shift();
  assert.doesNotThrow(() => validateManifest(isolated));
});

test("decision authority requires minor 7 and cannot borrow chat or another model", async () => {
  for (const change of [
    value => { value.contract.version.minor = 5; },
    value => { value.contract.version.minor = 6; },
    value => { value.providers[0].models[1].protocols = ["chat"]; },
    value => { value.providers[0].models[1].operations = []; },
    value => { value.providers[0].models[0].operations = ["systemone"]; },
    value => { value.providers[0].models[1].operations = ["systemone", "systemone"]; },
    value => { value.providers[0].models[1].operations = ["unknown"]; },
    value => { value.providers[0].models[1].operations = "systemone"; },
    value => { value.providers[0].operations = []; },
    value => { value.contract.capabilities = ["outbound-executor"]; },
  ]) {
    const value = decisionManifest();
    change(value);
    assert.throws(() => validateManifest(value));
  }
  let calls = 0;
  const manifest = decisionManifest();
  manifest.providers[0].operations.push("embeddings");
  const module = validateModule({
    manifest,
    handlers: {
      "provider.execute": async () => { calls++; return { status: 200 }; },
      "provider.models": async () => ({ models: [] }),
    },
  });
  const execute = params => module.handlers.get("provider.execute")({}, { params, scope: { provider: "judge" } }, {});
  for (const params of [
    { protocol: "chat", operation: "systemone", model: "decision" },
    { protocol: "", operation: "systemone", model: "" },
    { protocol: "chat", operation: "embeddings", model: "decision" },
  ]) assert.throws(() => execute(params), error => error.outcome === "not-started");
  assert.equal(calls, 0);
  await execute({ protocol: "", operation: "systemone", model: "decision" });
  assert.equal(calls, 1);
});

test("decision missing null and empty wire defaults never inherit chat or grant operations", () => {
  for (const field of ["protocols", "operations"]) {
    for (const form of ["missing", "null", "empty"]) {
      const value = decisionManifest();
      const model = value.providers[0].models[1];
      delete model[field];
      if (form === "null") model[field] = null;
      if (form === "empty") model[field] = [];
      if (field === "protocols") assert.doesNotThrow(() => validateManifest(value), `${field}/${form}`);
      else assert.throws(() => validateManifest(value), `${field}/${form}`);
    }
  }
});
