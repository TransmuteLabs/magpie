import assert from "node:assert/strict";
import { test } from "node:test";
import { validateCatalogAccess, validateCatalogRead, validateCatalogProjection, serviceManifest } from "../index.js";
import { harness, moduleFor } from "./helpers.js";

const declared = () => ({ contract: { version: { minor: 8 }, capabilities: [] }, catalogAccess: { scope: "inventory" } });
const projection = () => ({
  readAt: 1, accounts: [{
    ref: "a".repeat(64), provider: "vendor", kind: "account",
    models: [{ id: "vendor/model", contextWindow: 256000 }],
    quota: { known: true, windows: [{ name: "daily", models: ["vendor/model"], used: 0 }] },
  }, {
    ref: "b".repeat(64), provider: "other", kind: "key", models: [], quota: { known: false },
  }],
});

test("catalog projection permission is explicit, scoped and negotiated", () => {
  const manifest = declared();
  assert.doesNotThrow(() => validateCatalogRead(manifest, {}, {}));
  assert.throws(() => validateCatalogRead(manifest, {}, { provider: "forged" }));
  manifest.contract.version.minor = 7;
  assert.throws(() => validateCatalogAccess(manifest));
  manifest.contract.version.minor = 8;
  manifest.catalogAccess.scope = "selected";
  assert.throws(() => validateCatalogAccess(manifest));
  manifest.contract.capabilities = ["account-auth"];
  assert.doesNotThrow(() => validateCatalogAccess(manifest));
  assert.throws(() => validateCatalogRead(manifest, {}, {}));
  assert.doesNotThrow(() => validateCatalogRead(manifest, { provider: "vendor", account: "selected", generation: 1 }, {}));
});

test("catalog projection retains measured zero, unknown and account applicability", () => {
  assert.doesNotThrow(() => validateCatalogProjection(projection()));
  for (const mutate of [
    value => { value.readAt = 0; },
    value => { value.readAt = 2 ** 53; },
    value => { value.accounts[1].ref = value.accounts[0].ref; },
    value => { value.accounts[0].quota.known = false; },
    value => { value.accounts[0].quota.windows[0].models = ["foreign/model"]; },
    value => { value.accounts[0].quota.windows[0].used = NaN; },
    value => { value.accounts[0].models[0].contextWindow = -1; },
    value => { value.accounts[0].quota.asOf = 2 ** 53; },
    value => { value.accounts[0].quota.windows[0].resetsAt = 2 ** 53; },
    value => { value.accounts[0].quota.windows[0].models = []; },
  ]) {
    const value = projection();
    mutate(value);
    assert.throws(() => validateCatalogProjection(value));
  }
});

test("private service inherits explicit inventory permission without inventing legacy authority", () => {
  const root = {
    schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 8 }, capabilities: ["module-rpc"] },
    catalogAccess: { scope: "inventory" },
    actions: [{ id: "read", component: "backend", inputSchema: {}, effect: "read" }],
    services: [{ id: "backend", runtime: "exec", entry: "backend", start: "lazy",
      contract: { id: "catalog", version: 1 }, methods: ["read"] }],
  };
  const service = serviceManifest(root, "backend");
  assert.deepEqual(service.catalogAccess, { scope: "inventory" });
  assert.equal(service.contract.version.minor, 8);
  delete root.catalogAccess;
  const legacy = serviceManifest(root, "backend");
  assert.equal(legacy.catalogAccess, undefined);
  assert.equal(legacy.contract.version.minor, 1);
});

test("worker catalog callback inherits the live parent and carries no credential", { timeout: 10000 }, async () => {
  const module = moduleFor(async (_context, _call, stream) => stream.hostCall("catalog.read", {}));
  module.manifest.contract.version.minor = 8;
  module.manifest.catalogAccess = { scope: "inventory" };
  const worker = harness(module);
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "0", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "parent", method: "operation.status", scope: { plugin: "fixture" },
      auth: { token: "private" } });
    const callback = await worker.received.next();
    assert.equal(callback.method, "catalog.read");
    assert.deepEqual(callback.scope, { plugin: "fixture" });
    assert.equal(callback.auth, undefined);
    worker.send({ kind: "host.result", id: callback.id, parent: callback.parent, result: projection() });
    assert.deepEqual((await worker.received.next()).result, projection());
  } finally { await worker.close(); }
});
