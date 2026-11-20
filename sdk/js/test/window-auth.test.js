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

test("provider sign-in negotiates its own capability without a window or new wire version", () => {
  const manifest = source();
  delete manifest.window;
  manifest.contract.capabilities.push("provider-signin");
  manifest.authBindings[0].from = "provider:signin";
  Object.assign(manifest.authBindings[0].methods[0], { label: "Connect", fields: [
    { key: "key", label: "Key", type: "secret" },
    { key: "site", label: "Site", type: "select", choices: [{ value: "main", label: "Main" }] },
  ] });
  assert.equal(validateManifest(manifest).authBindings[0].methods[0].fields[1].choices[0].value, "main");
  assert.equal(publicManifest(manifest).authBindings, undefined);
  for (const mutate of [
    value => { value.contract.capabilities.pop(); },
    value => { value.contract.capabilities = ["provider-signin"]; },
    value => { delete value.authBindings[0].methods[0].label; },
    value => { value.authBindings[0].from = "window:page"; },
    value => { value.authBindings.push({ ...value.authBindings[0], id: "other" }); },
    value => { value.authBindings[0].methods[0].fields[1].choices.push({ value: "main", label: "Duplicate" }); },
    value => { value.authBindings[0].methods[0].fields[0].key = "constructor"; },
  ]) {
    const broken = structuredClone(manifest);
    mutate(broken);
    assert.throws(() => validateManifest(broken));
  }
});
