import assert from "node:assert/strict";
import { test } from "node:test";
import * as SDK from "../index.js";
import { harness } from "./helpers.js";

export function compositeManifest() {
  return {
    schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
    actions: [
      { id: "read", inputSchema: { type: "object" }, effect: "read" },
      { id: "write", inputSchema: true, effect: "write" },
      { id: "backend.run", component: "backend", inputSchema: {}, effect: "write" },
    ],
    services: [{
      id: "backend", runtime: "exec", entry: "bin/backend", contract: { id: "backend", version: 1 },
      methods: ["backend.run"], start: "lazy", ready: { httpEndpoints: ["ui"], timeoutMs: 1000 },
    }],
    compatibility: { hostAPI: "^1.1", nativeABI: 1, platforms: ["darwin/arm64", "linux/amd64"] },
    window: { metadata: "ui/package.json" },
    bindings: [
      { id: "page.read", from: "window:page", action: "read" },
      { id: "controller.run", from: "primary", action: "backend.run" },
      { id: "page.service", from: "window:panel:0", service: "backend", endpoint: "ui" },
    ],
  };
}

const configure = (id = "configure", extra = {}) => ({
  kind: "call", id, method: "component.configure",
  params: { component: "primary", instance: "instance", contract: {}, config: { version: "2", values: { mode: "first" } }, ...extra },
});

test("current SDK preserves API 1.1 primary and service authority projections", () => {
  assert.deepEqual(SDK.CONTRACT.version, { major: 1, minor: 2 });
  assert.equal(SDK.CONTRACT.capabilities.length, 11);
  const root = SDK.validateManifest(compositeManifest());
  assert.deepEqual(SDK.requiredMethods(root), ["module.call"]);
  const view = SDK.publicManifest(root);
  for (const key of ["runtime", "entry", "args", "services", "window", "bindings", "compatibility"]) assert.equal(view[key], undefined);
  assert.deepEqual(view.actions.map(action => action.id), ["read", "write"]);
  const service = SDK.serviceManifest(root, "backend");
  assert.equal(service.id, root.id);
  assert.deepEqual(service.component, { id: "backend", contract: { id: "backend", version: 1 } });
  assert.deepEqual(service.actions.map(action => action.id), ["backend.run"]);
  assert.deepEqual(SDK.publicManifest(service).actions, service.actions);
  assert.throws(() => SDK.serviceManifest(root, "missing"), SDK.SDKError);
  assert.throws(() => { service.actions[0].inputSchema.bad = true; }, TypeError);
});

test("module actions require configure, auto-dispatch input/action and reject streaming even if caught", async () => {
  const worker = harness({
    manifest: compositeManifest(),
    actions: {
      read(ctx, call) { assert.equal(ctx.module.config.version, "2"); return { action: call.action, input: call.params }; },
      async write(ctx, call, stream) {
        if (call.params.bad) {
          await assert.rejects(stream.emit("data", ""), error => error.code === "invalid_params");
          return "must not succeed";
        }
        await stream.emit("accepted", { operationId: call.scope.operation });
        return "written";
      },
    },
  });
  try {
    const request = (id, action, input = {}) => ({ kind: "call", id, method: "module.call", params: { action, input } });
    worker.send(request("early", "read"));
    assert.equal((await worker.received.next()).error.code, "not_ready");
    worker.send(configure());
    assert.deepEqual((await worker.received.next()).result.endpoints, {});
    worker.send(request("read", "read", { value: 7 }));
    assert.deepEqual((await worker.received.next()).result, { action: "read", input: { value: 7 } });
    worker.send(request("foreign", "backend.run"));
    assert.equal((await worker.received.next()).error.code, "method_not_found");
    worker.send(request("malformed", 7));
    assert.equal((await worker.received.next()).error.code, "invalid_params");
    worker.send(request("bad", "write", { bad: true }));
    assert.equal((await worker.received.next()).error.code, "invalid_params");
    worker.send({ ...request("write", "write"), scope: { operation: "operation" } });
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    worker.send({ kind: "ack", id: accepted.id, seq: accepted.seq });
    assert.equal((await worker.received.next()).result, "written");
  } finally { await worker.close(); }
});

test("composite descriptors reject invalid ownership, DAGs, bindings, paths and compatibility", () => {
  for (const change of [
    value => { value.contract.version.minor = 0; },
    value => { value.services[0].runtime = "native"; },
    value => { value.services[0].entry = "../backend"; },
    value => { value.services[0].entry = "bin/backend?arg"; },
    value => { value.services[0].start = "sometimes"; },
    value => { value.services[0].ready.timeoutMs = 300001; },
    value => { value.services[0].dependsOn = ["missing"]; },
    value => { value.services[0].dependsOn = ["backend"]; },
    value => { value.services.push({ ...value.services[0], id: "second", dependsOn: ["backend"] }); value.services[0].dependsOn = ["second"]; },
    value => { value.services[0].methods.push("backend.run"); },
    value => { value.services[0].methods = ["undeclared"]; },
    value => { value.services[0].ready.httpEndpoints = ["ui", "ui"]; },
    value => { value.services[0].contract.version = 0; },
    value => { value.actions[0].component = "backend"; },
    value => { value.actions[2].component = "missing"; },
    value => { value.actions[0].effect = "stream"; },
    value => { value.actions[0].inputSchema = []; },
    value => { value.actions.push(value.actions[0]); },
    value => { value.bindings[0].from = "backend"; },
    value => { value.bindings[0].action = "unknown"; },
    value => { value.bindings[0].service = "backend"; },
    value => { value.bindings[2].endpoint = "undeclared"; },
    value => { value.window.metadata = "ui/other.json"; },
    value => { value.compatibility.hostAPI = "^2"; },
    value => { value.compatibility.platforms = ["darwin/mips"]; },
    value => { value.compatibility.platforms = ["constructor/amd64"]; },
    value => { value.compatibility.platforms.push(value.compatibility.platforms[0]); },
  ]) {
    const value = compositeManifest();
    change(value);
    assert.throws(() => SDK.validateManifest(value), SDK.SDKError);
  }
  const service = structuredClone(SDK.serviceManifest(compositeManifest(), "backend"));
  assert.throws(() => SDK.validateManifest({ ...service, compatibility: { hostAPI: "^1.1" } }), SDK.SDKError);
  assert.throws(() => SDK.validateManifest({ ...service, actions: [{ id: "foreign", inputSchema: {}, effect: "read" }] }), SDK.SDKError);
  assert.doesNotThrow(() => SDK.checkCompatibility(compositeManifest(), "darwin/arm64"));
  assert.throws(() => SDK.checkCompatibility(compositeManifest(), "windows/amd64"), SDK.SDKError);
  assert.throws(() => SDK.checkCompatibility({ ...compositeManifest(), compatibility: { hostAPI: "^1", nativeABI: 2 } }, "darwin/arm64"), SDK.SDKError);
});

test("module service budget counts every eager and lazy component even with a shared binary", () => {
  assert.equal(SDK.MAX_MODULE_SERVICES, 16);
  const value = compositeManifest();
  for (let index = 1; index < 16; index++) value.services.push({
    id: "service-" + index, runtime: "bun", entry: "bin/backend",
    contract: { id: "backend", version: 1 }, methods: [], start: index % 2 ? "eager" : "lazy",
  });
  assert.doesNotThrow(() => SDK.validateManifest(value));
  value.services.push({ ...value.services[15], id: "service-16" });
  assert.throws(() => SDK.validateManifest(value), SDK.SDKError);
});
