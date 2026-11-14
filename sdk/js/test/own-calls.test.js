import assert from "node:assert/strict";
import { test } from "node:test";
import { appendOwnCall, requiredMethods, validateManifest, validateOwnCall, validateOutsideCallsRequest, validateOutsideCallsResult } from "../index.js";
import { harness } from "./helpers.js";

function fixture() {
  const manifest = {
    schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth", "usage-reporter"] },
    providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"], calls: true }],
  };
  const handlers = Object.fromEntries(requiredMethods(manifest).map(method => [method, async () => ({})]));
  return { manifest, handlers };
}

const call = { kind: "cache_keepalive", model: "model", status: 200, usage: { facts: { inputTokens: 0, cost: { amount: 0, currency: "USD" } } } };
const scope = { plugin: "fixture", provider: "vendor", account: "alice", generation: 2 };
const request = { since: 1700000000000, until: 1700000060000 };

test("own calls and outside readers negotiate separately and refuse arbitrary identities", () => {
  const { manifest } = fixture();
  assert.doesNotThrow(() => validateManifest(manifest));
  assert.ok(requiredMethods(manifest).includes("provider.calls"));
  assert.doesNotThrow(() => validateOwnCall(manifest, { call }));
  assert.doesNotThrow(() => validateOutsideCallsRequest(manifest, scope, request));
  assert.doesNotThrow(() => validateOutsideCallsResult(manifest, request, { calls: [{ ...call, id: "vendor-id", time: request.since }] }));
  for (const change of [{ account: "bob" }, { source: "reported" }, { callerKeyId: "forged" }])
    assert.throws(() => validateOwnCall(manifest, { call, ...change }));
  for (const change of [{ kind: "" }, { status: 600 }, { model: "bad model" }, { usage: { facts: { account: "bob" } } }])
    assert.throws(() => validateOwnCall(manifest, { call: { ...call, ...change } }));
  assert.doesNotThrow(() => validateOwnCall(manifest, { call: { ...call,
    usage: { facts: { inputTokens: Number.MAX_SAFE_INTEGER, deferred: { inputTokens: 0 } } } } }));
  assert.throws(() => validateOwnCall(manifest, { call: { ...call,
    usage: { facts: { inputTokens: Number.MAX_SAFE_INTEGER, deferred: { inputTokens: 1 } } } } }));
  assert.throws(() => validateOutsideCallsRequest(manifest, scope, { ...request, account: "bob" }));
  assert.throws(() => validateOutsideCallsRequest(manifest, { ...scope, operation: "inference" }, request));
  for (const result of [{}, { calls: null }, { calls: [], account: "bob" }, { calls: [{ ...call, id: "x", time: request.until + 1 }] }])
    assert.throws(() => validateOutsideCallsResult(manifest, request, result));
  const old = { ...manifest, contract: { ...manifest.contract, version: { major: 1, minor: 7 } } };
  assert.throws(() => validateManifest(old));
  assert.throws(() => validateOwnCall(old, { call }));
});

test("actual worker own-call callback keeps scope and omits private auth", async () => {
  const module = fixture();
  module.handlers["provider.execute"] = async (_context, _call, stream) => appendOwnCall(stream, call);
  const worker = harness(module);
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "own-parent", method: "provider.execute", scope, auth: { token: "private-fixture-token" } });
    const callback = await worker.received.next();
    assert.equal(callback.kind, "host.call");
    assert.equal(callback.method, "usage.append");
    assert.deepEqual(callback.scope, scope);
    assert.equal(callback.auth, undefined);
    assert.equal(JSON.stringify(callback).includes("private-fixture-token"), false);
    worker.send({ kind: "host.result", id: callback.id, parent: callback.parent, result: { accepted: true } });
    assert.deepEqual((await worker.received.next()).result, { accepted: true });
  } finally { await worker.close(); }
});

test("actual outside reader rejects an invalid later page instead of reporting an empty window", async () => {
  const module = fixture();
  module.handlers["provider.calls"] = async () => ({ calls: null });
  const worker = harness(module);
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "history", method: "provider.calls", scope, params: request });
    const result = await worker.received.next();
    assert.equal(result.kind, "result");
    assert.equal(result.error.code, "invalid_params");
  } finally { await worker.close(); }
});

test("actual outside reader cannot stream a partial history page", async () => {
  const module = fixture();
  module.handlers["provider.calls"] = async (_context, _call, stream) => {
    await stream.emit("usage", { inputTokens: 1 });
    return { calls: [] };
  };
  const worker = harness(module);
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "history", method: "provider.calls", scope, params: request });
    const result = await worker.received.next();
    assert.equal(result.kind, "result");
    assert.equal(result.error.code, "invalid_params");
  } finally { await worker.close(); }
});
