import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, publicManifest } from "../index.js";

function source() {
  return {
    schema: 1, id: "auth-window", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 5 }, capabilities: ["module-rpc", "account-auth", "model-discovery"] },
    providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"] }],
    window: { metadata: "package.json" },
    authBindings: [{ id: "signin", from: "window:page", provider: "vendor", methods: [{ id: "browser", mode: "poll" }] }],
  };
}

test("window authentication declares provider and polling transport separately", () => {
  const value = validateManifest(source());
  assert.equal(value.authBindings[0].methods[0].mode, "poll");
  assert.equal(publicManifest(value).authBindings, undefined);
  for (const mutate of [
    value => { value.contract.version.minor = 4; },
    value => { value.contract.capabilities = ["module-rpc", "model-discovery"]; },
    value => { delete value.window; },
    value => { value.authBindings[0].provider = "foreign"; },
    value => { value.authBindings[0].from = "primary"; },
    value => { value.authBindings[0].methods[0].mode = "callback"; },
    value => { value.authBindings[0].methods = []; },
    value => { value.authBindings[0].methods.push(value.authBindings[0].methods[0]); },
    value => { value.authBindings.push(value.authBindings[0]); },
    value => { value.authBindings[0].reauthorize = "yes"; },
  ]) {
    const value = source();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
  const legacy = source();
  legacy.contract.version.minor = 4;
  delete legacy.authBindings;
  assert.doesNotThrow(() => validateManifest(legacy));
});
