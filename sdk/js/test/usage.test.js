import assert from "node:assert/strict";
import { test } from "node:test";
import { validateUsageResult, parseUsageRevision } from "../index.js";

test("usage revision and applicability require 1.4 without weakening unknown freshness", () => {
  const manifest = { contract: { version: { major: 1, minor: 4 } } };
  const value = { revision: "1", windows: [{ name: "ordinary", used: 100, notModels: ["model-OffPeak"] }, { name: "mcp", unknown: true, aside: true }] };
  assert.equal(validateUsageResult(manifest, value), true);
  assert.equal(validateUsageResult(manifest, { ...value, readDeferred: true }), false);
  assert.equal(validateUsageResult(manifest, { windows: [{ name: "quota", unknown: true }] }), false);
  assert.throws(() => parseUsageRevision("01"));
  assert.throws(() => validateUsageResult({ contract: { version: { major: 1, minor: 3 } } }, value));
  assert.throws(() => validateUsageResult(manifest, { windows: [{ name: "quota", used: 2, models: ["a"], notModels: ["b"] }] }));
  for (const value of [
    { revision: null },
    { revision: "" },
    { windows: [{ name: "quota", used: 1, aside: null }] },
    { windows: [{ name: "quota", used: 1, asOf: -1 }] },
  ]) assert.throws(() => validateUsageResult(manifest, value));
});
