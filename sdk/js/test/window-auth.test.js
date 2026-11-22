import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, publicManifest } from "../index.js";
import { harness } from "./helpers.js";

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

test("saved sign-in requires its capability and a secret record", () => {
  const manifest = source();
  delete manifest.window;
  manifest.contract.capabilities.push("provider-signin");
  manifest.authBindings[0].from = "provider:signin";
  manifest.authBindings[0].methods = [{ id: "saved-signin", mode: "poll", label: "Import",
    fields: [{ key: "record", label: "Saved account", type: "secret" }] }];
  assert.throws(() => validateManifest(manifest));
  manifest.contract.capabilities.push("provider-saved-signin");
  assert.ok(publicManifest(validateManifest(manifest)).contract.capabilities.includes("provider-saved-signin"));
  for (const mutate of [
    value => { value.contract.capabilities.splice(value.contract.capabilities.indexOf("provider-signin"), 1); },
    value => {
      value.window = { metadata: "package.json" };
      value.authBindings[0].from = "window:page";
      value.authBindings[0].methods = [{ id: "saved-signin", mode: "poll" }];
    },
    value => { value.authBindings[0].methods[0].fields[0].optional = true; },
    value => { value.authBindings[0].methods[0].fields[0].key = "other"; },
    value => { value.authBindings[0].methods[0].fields[0].type = "text"; },
  ]) {
    const broken = structuredClone(manifest);
    mutate(broken);
    assert.throws(() => validateManifest(broken));
  }
});

test("a saved-sign-in worker confirms cancellation after its handler has drained", async () => {
  const manifest = source();
  delete manifest.window;
  delete manifest.authBindings;
  manifest.runtime = "bun";
  manifest.contract.capabilities = ["account-auth", "provider-signin", "provider-saved-signin", "model-discovery"];
  let entered, aborted, release;
  const started = new Promise(resolve => { entered = resolve; });
  const canceled = new Promise(resolve => { aborted = resolve; });
  const drained = new Promise(resolve => { release = resolve; });
  const noop = async () => null;
  const worker = harness({ manifest, handlers: {
    "auth.start": noop, "auth.finish": noop, "auth.refresh": noop, "auth.logout": noop,
    "provider.models": async (context, call) => {
      if (call.id === "held") {
        const cancel = new Promise(resolve => context.signal.addEventListener("abort", resolve, { once: true }));
        entered();
        await cancel;
        aborted();
        await drained;
      }
      return { models: [] };
    },
  } });
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", contract: {}, instance: "saved-signin", config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "held", method: "provider.models", scope: { provider: "vendor" } });
    await started;
    worker.send({ kind: "cancel", id: "held" });
    await canceled;
    worker.send({ kind: "call", id: "independent", method: "provider.models", scope: { provider: "vendor" } });
    assert.equal((await worker.received.next()).id, "independent");
    release();
    const terminal = await worker.received.next();
    assert.equal(terminal.id, "held");
    assert.equal(terminal.error.code, "canceled");
  } finally {
    release();
    await worker.close();
  }
});
