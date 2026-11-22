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

test("usage timestamps fit the host's RFC 3339 year range", () => {
  const manifest = { contract: { version: { major: 1, minor: 4 } } };
  const lastMillisecond = Date.parse("9999-12-31T23:59:59.999Z");
  const window = { name: "quota", used: 12 };
  for (const usageAt of [
    value => ({ asOf: value, windows: [window] }),
    value => ({ windows: [{ ...window, asOf: value }] }),
    value => ({ windows: [{ ...window, resetsAt: value }] }),
  ]) {
    assert.doesNotThrow(() => validateUsageResult(manifest, usageAt(0)));
    assert.doesNotThrow(() => validateUsageResult(manifest, usageAt(lastMillisecond)));
    assert.throws(() => validateUsageResult(manifest, usageAt(null)));
    assert.throws(() => validateUsageResult(manifest, usageAt(-1)));
    assert.throws(() => validateUsageResult(manifest, usageAt(lastMillisecond + 1)));
  }
});
