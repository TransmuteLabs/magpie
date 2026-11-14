import assert from "node:assert/strict";
import { test } from "node:test";
import { CONTRACT, validateManifest, requiredMethods, publicManifest, serviceManifest, validateMiddlewareResult } from "../index.js";
import { validateModule } from "../manifest.js";
import { validateMiddlewareCall } from "../middleware.js";

function manifest() {
  return { schema: 1, id: "middleware.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 8 }, capabilities: ["gateway-middleware"] },
    middleware: { request: true, account: true, provider: true, event: true, response: true } };
}

test("middleware has explicit API8 root capability and no async/private inheritance", () => {
  assert.ok(CONTRACT.capabilities.includes("gateway-middleware"));
  assert.ok(CONTRACT.capabilities.includes("ledger-observer"));
  assert.ok(CONTRACT.capabilities.includes("session-storage-source"));
  assert.equal(new Set(CONTRACT.capabilities).size, 16);
  const root = validateManifest(manifest());
  assert.equal(root.hooks, undefined);
  assert.equal(requiredMethods(root).length, 5);
  for (const mutate of [
    value => { value.contract.version.minor = 7; }, value => { delete value.middleware; },
    value => { value.contract.capabilities = ["module-rpc"]; }, value => { value.middleware = {}; },
    value => { value.middleware.origins = ["unknown"]; }, value => { value.middleware.origins = ["local", "local"]; },
    value => { value.middleware.events = ["bad\n"]; }, value => { value.middleware.events = ["delta", "delta"]; },
    value => { value.middleware.event = false; value.middleware.events = ["delta"]; },
  ]) {
    const value = manifest();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
  const value = manifest();
  value.contract.capabilities.push("module-rpc");
  value.middleware.events = ["delta"];
  value.services = [{ id: "helper", runtime: "exec", entry: "worker", contract: { id: "fixture.helper", version: 1 }, start: "lazy" }];
  assert.equal(serviceManifest(value, "helper").middleware, undefined);
  const view = publicManifest(value);
  value.middleware.events[0] = "changed";
  assert.equal(view.middleware.events[0], "delta");
});

test("middleware DTOs keep scoped credentials out and restrict reject/skip/drop", async () => {
  const request = { scope: { plugin: "middleware.fixture" }, params: { body: { model: "allowed" }, context: { origin: "local" } } };
  assert.doesNotThrow(() => validateMiddlewareCall("middleware.request", request, manifest().middleware));
  for (const scope of [{ account: "private", generation: 1 }, { provider: "foreign" }, { session: "foreign" }, { operation: "write" }, { attempt: 1 }]) {
    assert.throws(() => validateMiddlewareCall("middleware.request", { ...request, scope }, manifest().middleware));
  }
  assert.throws(() => validateMiddlewareCall("middleware.request", { ...request, auth: {} }, manifest().middleware));
  assert.throws(() => validateMiddlewareCall("middleware.request", { ...request, params: { ...request.params, context: { origin: "remote" } } }, manifest().middleware));
  for (const value of [
    { body: null }, { state: "x".repeat(65536) }, { skip: true }, { drop: true }, { credentials: "private" },
    { reject: { status: 200, message: "invalid" } }, { reject: { status: 403, message: "bad\nmessage" } },
    { reject: { status: 403, message: "denied" }, body: {} },
  ]) assert.throws(() => validateMiddlewareResult("middleware.request", value));
  assert.throws(() => validateMiddlewareResult("middleware.event", { reject: { status: 403, message: "denied" } }));
  assert.doesNotThrow(() => validateMiddlewareResult("middleware.request", { reject: { status: 403, message: "denied" } }));
  assert.doesNotThrow(() => validateMiddlewareResult("middleware.account", { skip: true }));
  assert.doesNotThrow(() => validateMiddlewareResult("middleware.event", { drop: true }));
  const value = manifest();
  let calls = 0;
  const prepared = validateModule({ manifest: value, handlers: Object.fromEntries(requiredMethods(value).map(method => [method, () => { calls++; return {}; }])) });
  await assert.rejects(prepared.handlers.get("middleware.request")({}, { ...request, auth: {} }, {}));
  assert.equal(calls, 0);
});
