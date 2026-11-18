import assert from "node:assert/strict";
import test from "node:test";
import { validateManifest } from "../index.js";

function fixture(storage) {
  const source = {
    id: "history", agent: "vendor", codec: "vendor-history", transcript: true, resume: "agent",
    agentResume: { agent: "vendor-sdk", args: ["--resume", "{sessionId}"] },
  };
  if (storage) {
    source.roots = [{ slot: "home", homeRelativePath: ".vendor" }];
    source.families = [{ id: "history", primary: "history.sqlite" }];
  } else {
    source.container = "jsonl";
    source.stores = [{ slot: "home", homeRelativePath: ".vendor" }];
    source.layout = { kind: "flat", idFrom: "filename-stem" };
  }
  return {
    schema: 1, id: "resume.fixture", version: "1.0.0", runtime: "native", entry: "module.so",
    contract: { version: { major: 1, minor: 10 }, capabilities: ["agent-wiring", storage ? "session-storage-source" : "session-source"] },
    agents: [{ id: "vendor-sdk", name: "Vendor", bin: "vendor", profile: { target: "vendor" }, launch: {} }],
    [storage ? "storageSources" : "sessionSources"]: [source],
  };
}

for (const storage of [false, true]) {
  const key = storage ? "storageSources" : "sessionSources";
  test(`${key} resume uses an explicitly owned launch profile`, () => {
    const manifest = fixture(storage);
    const checked = validateManifest(manifest);
    manifest[key][0].agentResume.args[0] = "--changed";
    assert.equal(checked[key][0].agentResume.args[0], "--resume");
    for (const mode of ["none", "native"]) {
      const legacy = fixture(storage);
      legacy.contract.version.minor = 8;
      legacy[key][0].resume = mode;
      delete legacy[key][0].agentResume;
      assert.doesNotThrow(() => validateManifest(legacy));
    }
  });
  test(`${key} refuses foreign, stale and ambiguous resume declarations`, () => {
    for (const change of [
      value => { value.contract.version.minor = 9; },
      value => { value.agents[0].profile.target = "another"; },
      value => { value[key][0].agentResume.agent = "absent"; },
      value => { delete value.agents[0].launch; },
      value => { value[key][0].resume = "native"; },
      value => { value[key][0].agentResume.args = ["--resume"]; },
      value => { value[key][0].agentResume.args = ["{sessionId}", "{sessionId}"]; },
      value => { value[key][0].agentResume.args = ["--resume={sessionId}"]; },
      value => { value[key][0].agentResume.args = ["{path}", "{sessionId}"]; },
      value => { value[key][0].agentResume.args = ["--resume\n", "{sessionId}"]; },
      value => { value[key][0].agentResume.args = ["x".repeat(1025), "{sessionId}"]; },
      value => { value[key][0].agentResume.program = "foreign"; },
    ]) {
      const manifest = fixture(storage);
      change(manifest);
      assert.throws(() => validateManifest(manifest));
    }
  });
}
