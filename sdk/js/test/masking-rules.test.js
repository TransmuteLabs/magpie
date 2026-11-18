import assert from "node:assert/strict";
import test from "node:test";
import { requiredMethods, validateManifest } from "../index.js";

const manifest = () => ({
  schema: 1, id: "mask", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 15 }, capabilities: ["masking-rules"] },
  maskingRuleSets: [{ id: "team", rules: [
    { kind: "SDK_TOKEN", prefix: "acme-key-" },
    { kind: "SDK_TICKET", regex: "acct-[A-Z0-9]{8,12}" },
  ] }],
});

test("SDK masking rules are declarative and opt in at API 1.15", () => {
  assert.deepEqual(requiredMethods(validateManifest(manifest())), []);
  for (const mutate of [
    m => { m.contract.version.minor = 14; },
    m => { m.maskingRuleSets = []; },
    m => { m.maskingRuleSets[0].rules.push({ kind: "SDK_A", regex: "acct-[Z-A]{8}" }); },
    m => { m.maskingRuleSets[0].rules.push({ kind: "SECRET", prefix: "acct-" }); },
    m => { m.maskingRuleSets[0].rules.push({ kind: "SDK_A", regex: "acct-[a-z]{4,129}" }); },
    m => { m.maskingRuleSets[0].rules.push({ Kind: "SDK_A", prefix: "acct-" }); },
    m => { m.maskingRuleSets[0].rules.push({ kind: "SDK_A", regex: "acct-(x+)?" }); },
  ]) {
    const value = manifest();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
});
