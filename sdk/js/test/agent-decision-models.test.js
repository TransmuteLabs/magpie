import test from "node:test";
import assert from "node:assert/strict";
import { validateAgentDecisionModels } from "../agent-decision-models.js";
import { SDKError } from "../bytes.js";

const manifest = {
  contract: { version: { major: 1, minor: 9 }, capabilities: ["agent-wiring"] },
};
const model = { id: "mixed/judge", name: "Judge", provider: "mixed", endpoints: ["/v1/systemone", "/v1/decisions"] };

test("agent decision options preserve separate provider and group references and an empty catalog", () => {
  for (const models of [[], [model], [{ id: "group/judges", name: "Judges", group: true, endpoints: ["/v1/decisions"] }]]) {
    const before = JSON.stringify(models);
    validateAgentDecisionModels(manifest, models);
    assert.equal(JSON.stringify(models), before);
    validateAgentDecisionModels(manifest, JSON.parse(before));
  }
});

test("agent decision DTO refuses credentials, conversation endpoints and invalid variants", () => {
  for (const models of [
    null, {}, [null], [[]], [{}], [{ ...model, id: "" }], [{ ...model, name: " " }],
    [{ ...model, provider: undefined }], [{ ...model, group: true }], [{ ...model, group: "true" }],
    [{ ...model, endpoints: [] }], [{ ...model, endpoints: null }],
    [{ ...model, endpoints: ["/v1/chat/completions"] }],
    [{ ...model, endpoints: ["https://vendor.invalid/v1/systemone"] }],
    [{ ...model, endpoints: ["/v1/systemone?key=private"] }],
    [{ ...model, endpoints: ["/v1/systemone", "/v1/systemone"] }],
    [{ ...model, credentials: {} }], [{ ...model, key: "private" }], [{ ...model, account: "private" }],
  ]) {
    assert.throws(() => validateAgentDecisionModels(manifest, models), SDKError);
  }
});

test("agent decision options require API9 agent wiring", () => {
  for (const contract of [
    { version: { major: 1, minor: 8 }, capabilities: ["agent-wiring"] },
    { version: { major: 2, minor: 9 }, capabilities: ["agent-wiring"] },
    { version: { major: 1, minor: 9 }, capabilities: ["model-discovery"] },
    { version: { major: 1 }, capabilities: ["agent-wiring"] },
  ]) {
    assert.throws(() => validateAgentDecisionModels({ contract }, []), SDKError);
  }
});

test("agent decision validation is exported and packaged by the SDK entry", async () => {
  const sdk = await import("../index.js");
  assert.equal(sdk.validateAgentDecisionModels, validateAgentDecisionModels);
  const { readFile } = await import("node:fs/promises");
  const metadata = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"));
  assert.ok(metadata.files.includes("agent-decision-models.js"));
  assert.ok(metadata.files.includes("agent-decision-models.d.ts"));
});
