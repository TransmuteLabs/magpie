import assert from "node:assert/strict";
import { test } from "node:test";
import * as SDK from "../index.js";
import { validateModule } from "../manifest.js";
import { harness } from "./helpers.js";

const context = (origin = "local") => ({
  origin, protocol: "chat", model: "fixture", stream: false, path: "/v1/chat/completions",
  agent: "editor", session: "session", cwd: "/work", query: "question", kind: "",
});
const manifest = (hooks = { request: true, response: true, signIn: true }) => ({
  schema: 1, id: "hooks", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 2 }, capabilities: ["gateway-hooks"] }, hooks,
});
const params = method => method === "gateway.request" ? { body: { messages: [] }, context: context() }
  : method === "gateway.response" ? { request: {}, reply: {}, success: true, state: { run: 1 }, context: context() }
  : { event: { provider: "fixture", providerName: "Fixture", account: "Account", state: "ending", endsAt: "2026-11-13T10:00:00Z" } };
const methods = ["gateway.request", "gateway.response", "gateway.signin"];
async function configured(handlers, hooks) {
  const worker = harness({ manifest: manifest(hooks), handlers });
  worker.send({ kind: "call", id: "configure", method: "component.configure",
    params: { component: "primary", contract: {}, instance: "instance", config: { version: "1", values: {} } } });
  assert.equal((await worker.received.next()).error, undefined);
  return worker;
}
const call = (worker, id, method, input = params(method), extra = {}) =>
  worker.send({ kind: "call", id, method, params: input, scope: { plugin: "hooks", session: "session", operation: id }, ...extra });

test("SDK 1.2 advertises gateway hooks and accepts explicit host compatibility", () => {
  assert.deepEqual(SDK.CONTRACT.version, { major: 1, minor: 2 });
  assert.equal(SDK.CONTRACT.capabilities.length, 11);
  assert.equal(SDK.MAX_GATEWAY_STATE_BYTES, 64 << 10);
  const source = manifest();
  source.compatibility = { hostAPI: "^1.2" };
  const frozen = SDK.validateManifest(source);
  assert.deepEqual(SDK.requiredMethods(frozen), methods);
  source.hooks.origins = ["remote"];
  assert.equal(frozen.hooks.origins, undefined);
  assert.deepEqual(SDK.publicManifest(frozen).hooks, { request: true, response: true, signIn: true });
});

test("gateway descriptors reject missing authority, unsupported versions, flags, budgets and origins", () => {
  for (const hooks of [{}, null, true, { request: "yes" }, { request: true, response: 1 },
    { request: true, timeoutMs: 99 }, { request: true, timeoutMs: 30001 }, { request: true, timeoutMs: 100.5 },
    { request: true, origins: ["unknown"] }, { request: true, origins: ["local", "local"] },
    { request: true, origins: "local" }, { signIn: true, origins: ["remote"] }, { signIn: true, origins: ["browser"] }]) {
    assert.throws(() => SDK.validateManifest(manifest(hooks)), SDK.SDKError, JSON.stringify(hooks));
  }
  for (const minor of [0, 1]) {
    assert.throws(() => SDK.validateManifest({ ...manifest(), contract: { version: { major: 1, minor }, capabilities: ["gateway-hooks"] } }), SDK.SDKError);
    assert.throws(() => SDK.validateManifest({ ...manifest({ request: true }), contract: { version: { major: 1, minor }, capabilities: ["operation-control"] } }), SDK.SDKError);
  }
  assert.throws(() => SDK.validateManifest({ ...manifest(), hooks: undefined }), SDK.SDKError);
  assert.throws(() => SDK.validateManifest({ ...manifest(), contract: { version: { major: 1, minor: 2 }, capabilities: ["operation-control"] } }), SDK.SDKError);
  for (const timeoutMs of [0, 100, 30000]) {
    assert.doesNotThrow(() => SDK.validateManifest(manifest({ request: true, timeoutMs })));
  }
  assert.doesNotThrow(() => SDK.validateManifest(manifest({ signIn: true, origins: ["local", "remote"] })));
});

test("only declared gateway methods require handlers and service manifests do not inherit hooks", () => {
  assert.deepEqual(SDK.requiredMethods(SDK.validateManifest(manifest({ response: true }))), ["gateway.response"]);
  assert.throws(() => validateModule({ manifest: manifest({ request: true }), handlers: {} }), SDK.SDKError);
  assert.throws(() => validateModule({ manifest: manifest({ request: true }),
    handlers: { "gateway.request": () => null, "gateway.response": () => null } }), SDK.SDKError);
  const root = { ...manifest(), contract: { version: { major: 1, minor: 2 }, capabilities: ["gateway-hooks", "module-rpc"] },
    actions: [{ id: "helper.run", component: "helper", inputSchema: {}, effect: "read" }],
    services: [{ id: "helper", runtime: "exec", entry: "helper", contract: { id: "helper", version: 1 }, methods: ["helper.run"], start: "lazy" }] };
  const helper = SDK.serviceManifest(root, "helper");
  assert.equal(helper.hooks, undefined);
  assert.deepEqual(helper.contract.capabilities, ["module-rpc"]);
  assert.throws(() => SDK.validateManifest({ ...helper, hooks: { request: true } }), SDK.SDKError);
});

test("tool exposure requires API 1.2 and retains shared defaults for SDK 1.0 and 1.1", () => {
  for (const minor of [0, 1, 2]) {
    const value = { ...manifest(), hooks: undefined, contract: { version: { major: 1, minor }, capabilities: ["tool-provider"] },
      tools: [{ name: "tool", inputSchema: {} }] };
    assert.doesNotThrow(() => SDK.validateManifest(value));
    for (const exposure of ["shared", "local-owner"]) {
      const declared = { ...value, tools: [{ ...value.tools[0], exposure }] };
      if (minor < 2) assert.throws(() => SDK.validateManifest(declared), SDK.SDKError);
      else assert.equal(SDK.publicManifest(SDK.validateManifest(declared)).tools[0].exposure, exposure);
    }
    for (const exposure of ["public", "owner", true, 1, {}]) {
      assert.throws(() => SDK.validateManifest({ ...value, tools: [{ ...value.tools[0], exposure }] }), SDK.SDKError);
    }
  }
});

test("request hooks return detached body and state while response domain outcomes remain ordinary data", async () => {
  const worker = await configured({
    "gateway.request": (_ctx, request) => ({ body: { ...request.params.body, recalled: true }, state: { run: request.params.context.session } }),
    "gateway.response": () => ({ outcome: "domain-complete", stored: true }),
    "gateway.signin": () => null,
  });
  try {
    call(worker, "request", "gateway.request");
    assert.deepEqual((await worker.received.next()).result, { body: { messages: [], recalled: true }, state: { run: "session" } });
    call(worker, "response", "gateway.response");
    assert.deepEqual((await worker.received.next()).result, { outcome: "domain-complete", stored: true });
  } finally { await worker.close(); }
});

test("gateway hooks never receive selected account credentials or a populated account scope", async () => {
  let ran = 0;
  const worker = await configured(Object.fromEntries(methods.map(method => [method, (_ctx, request) => {
    ran++;
    assert.equal(Object.hasOwn(request, "auth"), false);
    return method === "gateway.request" ? {} : null;
  }])));
  try {
    let id = 0;
    for (const method of methods) {
      for (const extra of [{ auth: { token: "PRIVATE credential" } }, { scope: { provider: "provider" } },
        { scope: { account: "account" } }, { scope: { generation: 1 } }]) {
        call(worker, `refused-${++id}`, method, params(method), extra);
        const result = await worker.received.next();
        assert.equal(result.error.code, "permission_denied");
        assert.equal(JSON.stringify(result).includes("PRIVATE"), false);
      }
      call(worker, `allowed-${++id}`, method, params(method), { auth: null, scope: { generation: 0 } });
      assert.equal((await worker.received.next()).error, undefined);
    }
    assert.equal(ran, 3);
  } finally { await worker.close(); }
});

test("gateway origin defaults and explicit origin declarations are enforced before handlers", async () => {
  for (const origins of [undefined, ["remote", "gateway-key", "browser"]]) {
    let ran = 0;
    const worker = await configured({
      "gateway.request": () => { ran++; return {}; },
      "gateway.response": () => { ran++; return null; },
    }, { request: true, response: true, ...(origins ? { origins } : {}) });
    try {
      for (const method of ["gateway.request", "gateway.response"]) for (const origin of ["local", "magpie-key", "gateway-key", "remote", "browser", "unknown"]) {
        call(worker, `${method}-${origin}`, method, { ...params(method), context: context(origin) });
        const frame = await worker.received.next();
        if ((origins ?? ["local", "magpie-key"]).includes(origin)) assert.equal(frame.error, undefined);
        else assert.equal(frame.error.code, origin === "unknown" ? "invalid_params" : "permission_denied");
      }
      assert.equal(ran, 2 * (origins ?? ["local", "magpie-key"]).length);
    } finally { await worker.close(); }
  }
});

test("gateway hooks reject streaming even when the handler catches Emit errors", async () => {
  let event;
  const worker = await configured(Object.fromEntries(methods.map(method => [method, async (_ctx, request, stream) => {
    try { await stream.emit(event, event === "accepted" ? { operationId: request.scope.operation } : ""); } catch {}
    return method === "gateway.request" ? {} : "must not succeed";
  }])));
  try {
    for (const method of methods) for (event of method === "gateway.request" ? ["accepted", "data", "progress"] : ["data", "progress"]) {
      call(worker, `${method}-${event}`, method);
      const frame = await worker.received.next();
      assert.equal(frame.kind, "result");
      assert.equal(frame.error.code, "invalid_params");
    }
  } finally { await worker.close(); }
});

test("write hook acceptance waits for ACK and host callbacks retain only the parent authority", async () => {
  const worker = await configured({ "gateway.response": async (_ctx, request, stream) => {
    await stream.emit("accepted", { operationId: request.scope.operation });
    return stream.hostCall("state.cas", { key: "saved", version: "0", value: request.params.state });
  } }, { response: true });
  try {
    call(worker, "write", "gateway.response");
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    assert.equal(worker.received.frames.length, 0);
    worker.send({ kind: "ack", id: "write", seq: accepted.seq });
    const request = await worker.received.next();
    assert.equal(request.method, "state.cas");
    assert.equal(request.parent, "write");
    assert.deepEqual(request.scope, { plugin: "hooks", session: "session", operation: "write" });
    assert.equal(Object.hasOwn(request, "auth"), false);
    worker.send({ kind: "host.result", id: request.id, parent: "write", result: { swapped: true, version: "1" } });
    assert.deepEqual((await worker.received.next()).result, { swapped: true, version: "1" });
  } finally { await worker.close(); }
});

test("gateway state has an exact 64 KiB UTF-8 JSON bound on request output and response input", async () => {
  let output;
  const worker = await configured({ "gateway.request": () => output, "gateway.response": () => null }, { request: true, response: true });
  try {
    const limit = 64 << 10;
    for (const [state, allowed] of [["x".repeat(limit - 2), true], ["x".repeat(limit - 1), false],
      ["é".repeat((limit - 2) / 2), true], ["é".repeat(limit / 2), false]]) {
      output = { state };
      call(worker, `request-${state.length}-${allowed}`, "gateway.request");
      const request = await worker.received.next();
      assert.equal(request.error?.code, allowed ? undefined : "resource_exhausted");
      call(worker, `response-${state.length}-${allowed}`, "gateway.response", { ...params("gateway.response"), state });
      const response = await worker.received.next();
      assert.equal(response.error?.code, allowed ? undefined : "resource_exhausted");
    }
    output = null;
    call(worker, "nil-result", "gateway.request");
    assert.deepEqual((await worker.received.next()).result, {});
    output = { body: [] };
    call(worker, "array-body", "gateway.request");
    assert.equal((await worker.received.next()).error.code, "invalid_params");
  } finally { await worker.close(); }
});

test("gateway response state limits count original JSON escapes and nested whitespace", async () => {
  let ran = 0;
  const worker = await configured({ "gateway.response": () => { ran++; return null; } }, { response: true });
  try {
    const states = [
      [`"${"\\u0061".repeat(10922)}"`, true],
      [`"${"\\u0061".repeat(10923)}"`, false],
      [`{"value":${" ".repeat(64 << 10)}1}`, false],
    ];
    for (const [index, [state, allowed]] of states.entries()) {
      worker.input.write(`{"kind":"call","id":"raw-${index}","method":"gateway.response","params":{"request":{},"reply":{},"success":true,"state":${state},"context":${JSON.stringify(context())}}}\n`);
      const frame = await worker.received.next();
      assert.equal(frame.error?.code, allowed ? undefined : "resource_exhausted");
    }
    assert.equal(ran, 1);
  } finally { await worker.close(); }
});

test("gateway DTO validation rejects malformed exchanges and sanitized sign-in events", async () => {
  const worker = await configured(Object.fromEntries(methods.map(method => [method, () => null])));
  try {
    const invalid = [
      ["gateway.request", { body: [], context: context() }],
      ["gateway.request", { body: {}, context: { ...context(), credential: "private" } }],
      ["gateway.response", { ...params("gateway.response"), reply: [], success: true }],
      ["gateway.response", { ...params("gateway.response"), request: null }],
      ["gateway.response", { ...params("gateway.response"), success: "true" }],
      ...[{ state: "unknown" }, { provider: "bad provider" }, { account: "label\ncontrol" },
        { endsAt: "tomorrow" }, { endsAt: "2026-02-30T10:00:00Z" }, { endsAt: 123 }].map(patch =>
        ["gateway.signin", { event: { ...params("gateway.signin").event, ...patch } }]),
    ];
    for (const [index, [method, input]] of invalid.entries()) {
      call(worker, `invalid-${index}`, method, input);
      assert.equal((await worker.received.next()).error.code, "invalid_params");
    }
    call(worker, "failed-reply", "gateway.response", { ...params("gateway.response"), success: false, reply: null });
    assert.equal((await worker.received.next()).error, undefined);
    for (const endsAt of ["2024-02-29T10:00:00.123456789+03:00", ""]) {
      call(worker, `date-${endsAt}`, "gateway.signin", { event: { ...params("gateway.signin").event, endsAt } });
      assert.equal((await worker.received.next()).error, undefined);
    }
  } finally { await worker.close(); }
});

test("cancellation interrupts hook host waits and preserves the configured instance", async () => {
  const worker = await configured({ "gateway.request": async (_ctx, _request, stream) => {
    await stream.hostCall("state.get", { key: "pending" });
    return {};
  } }, { request: true });
  try {
    call(worker, "cancel", "gateway.request");
    const pending = await worker.received.next();
    assert.equal(pending.kind, "host.call");
    worker.send({ kind: "cancel", id: "cancel" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send({ kind: "host.result", id: pending.id, parent: "cancel", result: null });
    worker.send({ kind: "call", id: "ready", method: "component.ready", params: {} });
    assert.equal((await worker.received.next()).result.instance, "instance");
  } finally { await worker.close(); }
});
