import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, validateAccountSelection } from "../index.js";

function source() {
  return {
    schema: 1, id: "account-window", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 5 }, capabilities: ["module-rpc", "account-auth", "model-discovery"] },
    providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"] }],
    actions: [{ id: "inspect", effect: "read", inputSchema: {} }],
    window: { metadata: "package.json" },
    bindings: [{ id: "page.inspect", from: "window:page", action: "inspect", provider: "vendor" }],
  };
}

test("selected account bindings require negotiated same-module provider grants", () => {
  assert.doesNotThrow(() => validateManifest(source()));
  for (const mutate of [
    value => { value.contract.version.minor = 4; },
    value => { value.contract.capabilities = ["module-rpc", "model-discovery"]; },
    value => { value.bindings[0].provider = "foreign"; },
    value => { value.bindings[0].from = "primary"; },
    value => { value.bindings[0].action = ""; },
    value => { value.bindings[0].provider = 123; },
  ]) {
    const value = source();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
  const legacy = source();
  legacy.contract.version.minor = 4;
  delete legacy.bindings[0].provider;
  assert.doesNotThrow(() => validateManifest(legacy));
});

test("account selection keeps uint64 generation strings and refuses supplied authority", () => {
  for (const expectedGeneration of ["1", "9007199254740993", "18446744073709551615"]) {
    const value = { id: "user@example.invalid", expectedGeneration };
    assert.equal(validateAccountSelection(value), value);
  }
  for (const value of [
    null, {}, { id: "one", expectedGeneration: "0" }, { id: "one", expectedGeneration: "01" },
    { id: "one", expectedGeneration: 1 }, { id: "one", expectedGeneration: "18446744073709551616" },
    { id: "one", expectedGeneration: "1", provider: "other" }, { id: "one", expectedGeneration: "1", auth: {} },
  ]) assert.throws(() => validateAccountSelection(value));
});
