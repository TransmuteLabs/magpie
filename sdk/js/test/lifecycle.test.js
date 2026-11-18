import assert from "node:assert/strict";
import { test } from "node:test";
import { SDKError, moduleContextFrom, serviceManifest } from "../index.js";
import { validateModule } from "../manifest.js";
import { harness } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "lifetime", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
  actions: [{ id: "read", inputSchema: true, effect: "read" }],
});
const config = (id = "configure", extra = {}) => ({
  kind: "call", id, method: "component.configure",
  params: { component: "primary", contract: {}, instance: "instance", config: { version: "2", values: { mode: "original" } }, ...extra },
});
const request = (id, method, params = {}) => ({ kind: "call", id, method, params });

test("configured lifetime outlives configure and snapshots isolate startup and each call", async () => {
  let lifetimeSignal, disposals = 0;
  const worker = harness({
    manifest: manifest(),
    start(ctx, value) {
      lifetimeSignal = ctx.signal;
      value.config.values.mode = "startup edit";
      value.services.backend.endpoints.ui = "http://127.0.0.1:9999/";
      value.services.extra = {};
      return { endpoints: { ui: "http://127.0.0.1:8080/" }, dispose() { disposals++; } };
    },
    actions: { read(ctx) {
      assert.equal(ctx.module.config.values.mode, "original");
      assert.equal(ctx.module.services.extra, undefined);
      assert.equal(ctx.module.services.backend.endpoints.ui, "http://127.0.0.1:8081/");
      const clone = moduleContextFrom(ctx);
      clone.config.values.mode = "call edit";
      clone.services.backend.endpoints.ui = "http://127.0.0.1:9998/";
      assert.equal(moduleContextFrom(ctx).config.values.mode, "original");
      assert.equal(moduleContextFrom(ctx).services.backend.endpoints.ui, "http://127.0.0.1:8081/");
      assert.throws(() => { ctx.module.config.values.mode = "shared edit"; }, TypeError);
      return ctx.module;
    } },
  });
  try {
    worker.send(config("configure", { services: { backend: {
      component: "backend", contract: { id: "backend", version: 1 }, instance: "child",
      configVersion: "2", endpoints: { ui: "http://127.0.0.1:8081/" },
    } } }));
    assert.deepEqual((await worker.received.next()).result, {
      component: "primary", contract: { id: "", version: 0 }, instance: "instance",
      configVersion: "2", endpoints: { ui: "http://127.0.0.1:8080/" },
    });
    assert.equal(lifetimeSignal.aborted, false, "terminal configure must not cancel successful startup");
    for (const id of ["one", "two"]) {
      worker.send(request(id, "module.call", { action: "read", input: null }));
      assert.equal((await worker.received.next()).result.config.version, "2");
    }
    worker.send(request("stop", "component.stop"));
    assert.deepEqual((await worker.received.next()).result, {});
    assert.equal(lifetimeSignal.aborted, true);
    assert.equal(disposals, 1);
    worker.send(request("again", "component.stop"));
    assert.deepEqual((await worker.received.next()).result, {});
    assert.equal(disposals, 1);
    worker.send(request("closed", "module.call", { action: "read", input: null }));
    assert.equal((await worker.received.next()).error.code, "canceled");
  } finally { await worker.close(); }
  assert.equal(disposals, 1);
});

test("configure is idempotent, immutable, and control methods reject unknown parameters", async () => {
  let starts = 0;
  const worker = harness({ manifest: manifest(), actions: { read() { return null; } }, start() { starts++; return {}; } });
  try {
    worker.send(request("early", "component.ready"));
    assert.equal((await worker.received.next()).error.code, "not_ready");
    worker.send(config());
    assert.equal((await worker.received.next()).result.instance, "instance");
    worker.send(config("same"));
    assert.equal((await worker.received.next()).result.instance, "instance");
    assert.equal(starts, 1);
    worker.send(config("changed", { config: { version: "3", values: {} } }));
    assert.equal((await worker.received.next()).error.code, "conflict");
    for (const method of ["component.ready", "component.stop"]) {
      worker.send(request(method, method, { credentials: "never accepted" }));
      assert.equal((await worker.received.next()).error.code, "invalid_params");
    }
  } finally { await worker.close(); }
});

test("configuration validates exact uint64 versions and dependency identity before starting", async () => {
  let starts = 0;
  const worker = harness({ manifest: manifest(), actions: { read() {} }, start() { starts++; return {}; } });
  try {
    for (const [index, extra] of [
      { instance: "bad instance" }, { component: "other" }, { contract: { id: "other", version: 1 } },
      { config: { version: "02", values: {} } }, { config: { version: "18446744073709551616", values: {} } },
      { config: { version: "1" } }, { services: { backend: { component: "other", contract: { id: "backend", version: 1 }, instance: "child", configVersion: "2" } } },
      { services: { backend: { component: "backend", contract: { id: "backend", version: 1 }, instance: "child", configVersion: "1" } } },
      { unknown: "not a control field" },
    ].entries()) {
      worker.send(config(`invalid-${index}`, extra));
      assert.equal((await worker.received.next()).error.code, "invalid_params");
    }
    assert.equal(starts, 0);
    worker.send(config("max", { config: { version: "18446744073709551615", values: { generation: 18446744073709551615n } } }));
    assert.equal((await worker.received.next()).result.configVersion, "18446744073709551615");
    assert.equal(starts, 1);
  } finally { await worker.close(); }
});

test("pending startup is not ready and canceled configure disposes a late startup result", async () => {
  let started, disposals = 0;
  const beginning = new Promise(resolve => { started = resolve; });
  const worker = harness({
    manifest: manifest(), actions: { read() {} },
    async start(ctx) {
      started();
      await new Promise(resolve => ctx.signal.addEventListener("abort", resolve, { once: true }));
      return { dispose() { disposals++; } };
    },
  });
  try {
    worker.send(config());
    await beginning;
    worker.send(request("pending", "component.ready"));
    assert.equal((await worker.received.next()).error.code, "not_ready");
    worker.send({ kind: "cancel", id: "configure" });
    assert.equal((await worker.received.next()).error.code, "canceled");
    worker.send(request("stop", "component.stop"));
    assert.deepEqual((await worker.received.next()).result, {});
    assert.equal(disposals, 1);
  } finally { await worker.close(); }
});

test("startup readiness validates loopback endpoints and sanitizes private failures", async () => {
  for (const endpoint of ["http://example.invalid:8080/", "http://127.0.0.1:0/", "http://localhost:8080/#fragment"]) {
    let disposals = 0;
    const worker = harness({
      manifest: manifest(), actions: { read() {} },
      start() { return { endpoints: { ui: endpoint }, dispose() { disposals++; } }; },
    });
    try {
      worker.send(config());
      const reply = await worker.received.next();
      assert.equal(reply.error.code, "invalid_params");
      assert.equal(disposals, 1);
    } finally { await worker.close(); }
  }
  const worker = harness({ manifest: manifest(), actions: { read() {} }, start() { throw new Error("SECRET startup credentials"); } });
  try {
    worker.send(config());
    const reply = await worker.received.next();
    assert.equal(reply.error.code, "internal");
    assert(!JSON.stringify(reply).includes("SECRET"));
  } finally { await worker.close(); }
});

test("service actions preserve exact parent authority through component.call without forwarding auth", async () => {
  const root = manifest();
  root.actions = [{ id: "backend.run", component: "backend", inputSchema: true, effect: "read" }];
  root.services = [{ id: "backend", runtime: "bun", entry: "backend.js", contract: { id: "backend", version: 1 }, start: "lazy", methods: ["backend.run"] }];
  const worker = harness({
    manifest: serviceManifest(root, "backend"),
    actions: { "backend.run": (ctx, call, stream) => {
      assert.equal(call.action, "backend.run");
      assert.deepEqual(call.auth, { token: "selected-auth" });
      assert.equal(ctx.module.component, "backend");
      return stream.hostCall("component.call", { binding: "next", input: call.params });
    } },
  });
  try {
    worker.send(config("configure", { component: "backend", contract: { id: "backend", version: 1 } }));
    assert.equal((await worker.received.next()).result.component, "backend");
    const scope = { plugin: "lifetime", provider: "provider", account: "account", generation: 18446744073709551615n, operation: "operation", attempt: 2 };
    worker.send({ ...request("parent", "module.call", { action: "backend.run", input: { value: 3 } }), scope, auth: { token: "selected-auth" } });
    const child = await worker.received.next();
    assert.equal(child.method, "component.call");
    assert.equal(child.parent, "parent");
    assert.deepEqual(child.scope, scope);
    assert.equal(child.auth, undefined);
    worker.send({ kind: "host.result", id: child.id, parent: "parent", result: { value: 4 } });
    assert.deepEqual((await worker.received.next()).result, { value: 4 });
  } finally { await worker.close(); }
});

test("SDK owns module.call and lifecycle while undeclared and service-owned handlers are rejected", () => {
  const module = { manifest: manifest(), actions: { read() {} } };
  assert.doesNotThrow(() => validateModule(module));
  assert.throws(() => validateModule({ ...module, handlers: { "module.call"() {} } }), SDKError);
  assert.throws(() => validateModule({ ...module, handlers: { "component.ready"() {} } }), SDKError);
  assert.throws(() => validateModule({ ...module, actions: {} }), SDKError);
  assert.throws(() => validateModule({ ...module, actions: { read() {}, foreign() {} } }), SDKError);
  assert.throws(() => validateModule({
    manifest: { ...manifest(), contract: { version: { major: 1, minor: 0 }, capabilities: ["operation-control"] }, actions: undefined },
    handlers: { "operation.status"() {}, "operation.cancel"() {} }, start() {},
  }), SDKError);
});

test("stop cancels active ACK and host waits, joins the disposer once, and retains the wire", async () => {
  let release, disposals = 0;
  const disposing = new Promise(resolve => { release = resolve; });
  const value = manifest();
  value.contract.capabilities.push("operation-control");
  const worker = harness({
    manifest: value,
    start() { return { async dispose() { disposals++; await disposing; } }; },
    handlers: {
      "operation.status": async (ctx, call, stream) => { await stream.emit("flush"); return null; },
      "operation.cancel": () => null,
    },
    actions: { read: (ctx, call, stream) => stream.hostCall("component.call", { binding: "next", input: null }) },
  });
  try {
    worker.send(config());
    await worker.received.next();
    worker.send(request("ack", "operation.status"));
    const event = await worker.received.next();
    worker.send(request("host", "module.call", { action: "read", input: null }));
    const child = await worker.received.next();
    worker.send(request("stop", "component.stop"));
    const canceled = [await worker.received.next(), await worker.received.next()];
    assert.deepEqual(canceled.map(reply => reply.id).sort(), ["ack", "host"]);
    assert(canceled.every(reply => reply.error.code === "canceled"));
    assert.equal(disposals, 1);
    assert(!worker.received.frames.some(reply => reply.id === "stop"));
    worker.send(request("stop-again", "component.stop"));
    release();
    const stopped = [await worker.received.next(), await worker.received.next()];
    assert.deepEqual(stopped.map(reply => reply.id).sort(), ["stop", "stop-again"]);
    assert(stopped.every(reply => reply.error === undefined && JSON.stringify(reply.result) === "{}"));
    worker.send({ kind: "ack", id: event.id, seq: event.seq });
    worker.send({ kind: "host.result", id: child.id, parent: child.parent, result: null });
    worker.send(request("describe", "sdk.describe"));
    assert.equal((await worker.received.next()).result.id, "lifetime");
  } finally { release(); await worker.close(); }
  assert.equal(disposals, 1);
});

test("concurrent instances sharing handlers keep opaque startup state local and off the wire", async () => {
  const states = new Map();
  const module = {
    manifest: manifest(),
    start(ctx, snapshot) {
      const state = { count: 0, instance: snapshot.instance, toJSON() { throw new Error("opaque state must not be serialized"); } };
      state.self = state;
      states.set(snapshot.instance, state);
      return { state };
    },
    actions: { read(ctx) {
      const state = ctx.module.state;
      assert.equal(moduleContextFrom(ctx).state, state);
      assert.equal(state, states.get(ctx.module.instance));
      assert.equal(Object.isFrozen(state), false);
      state.count++;
      return { count: state.count, instance: state.instance };
    } },
  };
  const first = harness(module), second = harness(module);
  try {
    first.send(config("first", { instance: "first" }));
    second.send(config("second", { instance: "second" }));
    for (const worker of [first, second]) {
      const ready = (await worker.received.next()).result;
      assert.equal(ready.state, undefined);
    }
    for (const [worker, instance] of [[first, "first"], [second, "second"]]) {
      worker.send(request("one", "module.call", { action: "read", input: null }));
      assert.deepEqual((await worker.received.next()).result, { count: 1, instance });
    }
    first.send(request("two", "module.call", { action: "read", input: null }));
    assert.deepEqual((await first.received.next()).result, { count: 2, instance: "first" });
    assert.equal(states.get("second").count, 1);
    assert.notEqual(states.get("first"), states.get("second"));
  } finally { await first.close(); await second.close(); }
});
