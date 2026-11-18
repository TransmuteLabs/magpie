import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest } from "../index.js";

function manifest(minor, defaults) {
  return {
    schema: 1, id: "defaults.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor }, capabilities: ["outbound-executor"] },
    providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"], ...defaults }],
  };
}

test("provider defaults require API9, preserve zero and reject invalid numbers", () => {
  for (const field of ["maxConcurrency", "maxAccounts"]) {
    assert.throws(() => validateManifest(manifest(8, { [field]: 3 })), /1\.9|API.?9|default/i);
    for (const value of [-1, 1001, 0.5, "3", true, NaN, Infinity]) {
      assert.throws(() => validateManifest(manifest(9, { [field]: value })));
    }
    for (const value of [undefined, null, 0, 3, 1000]) {
      assert.doesNotThrow(() => validateManifest(manifest(9, { [field]: value })));
    }
  }
});
