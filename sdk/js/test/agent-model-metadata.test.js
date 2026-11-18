import test from "node:test";
import assert from "node:assert/strict";
import { validateAgentModelMetadata } from "../agent-model-metadata.js";
import { SDKError } from "../bytes.js";
import { validateManifest } from "../manifest.js";

const manifest = {
  contract: { version: { major: 1, minor: 9 }, capabilities: ["agent-wiring"] },
};

test("effective agent metadata preserves unknown and empty effort knowledge", () => {
  for (const efforts of [null, [], ["low", "medium", "high"], ["high", "ultra"]]) {
    const value = { maxOutput: 393216, efforts };
    if (efforts?.length) value.defaultEffort = efforts.at(-1);
    validateAgentModelMetadata(manifest, value);
    assert.deepEqual(JSON.parse(JSON.stringify(value)), value);
  }
});

test("effective agent metadata refuses removed defaults and malformed facts", () => {
  for (const value of [
    null, [], {}, { efforts: "high" }, { efforts: ["high", "high"] },
    { efforts: ["automatic"] }, { efforts: [], defaultEffort: "high" },
    { efforts: null, defaultEffort: "high" }, { efforts: ["low", "high"], defaultEffort: "medium" },
    { efforts: null, maxOutput: -1 }, { efforts: null, maxOutput: 1.5 },
    { efforts: null, maxOutput: Number.MAX_SAFE_INTEGER + 1 }, { efforts: null, maxOutput: NaN },
    { efforts: null, credentials: {} },
  ]) {
    assert.throws(() => validateAgentModelMetadata(manifest, value), SDKError);
  }
});

test("effective agent metadata cannot be borrowed by old or unrelated contracts", () => {
  for (const contract of [
    { version: { major: 1, minor: 8 }, capabilities: ["agent-wiring"] },
    { version: { major: 1, minor: 9 }, capabilities: ["model-discovery"] },
    { version: { major: 2, minor: 9 }, capabilities: ["agent-wiring"] },
    { version: { major: 1 }, capabilities: ["agent-wiring"] },
  ]) {
    assert.throws(() => validateAgentModelMetadata({ contract }, { efforts: null }), SDKError);
  }
});

test("effective agent metadata is exported by the SDK package entry", async () => {
  const sdk = await import("../index.js");
  assert.equal(sdk.validateAgentModelMetadata, validateAgentModelMetadata);
});

test("SDK model discovery retains unknown and explicitly empty effort lists", () => {
  for (const minor of [0, 8, 9]) {
    for (const efforts of [null, []]) {
      const checked = validateManifest({
        schema: 1, id: "model-metadata", version: "1.0.0", runtime: "exec", entry: "worker",
        compatibility: minor === 0 ? undefined : { hostAPI: `^1.${minor}` },
        contract: { version: { major: 1, minor }, capabilities: ["outbound-executor", "model-discovery"] },
        providers: [{ id: "fixture", name: "Fixture", protocols: ["chat"], models: [{ id: "m", efforts }] }],
      });
      assert.deepEqual(checked.providers[0].models[0].efforts, efforts);
    }
  }
});
