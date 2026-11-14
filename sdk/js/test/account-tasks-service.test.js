import assert from "node:assert/strict";
import { test } from "node:test";
import { serviceManifest, publicManifest, requiredMethods, validateManifest, reportAccountTaskUsage,
  appendOwnCall, validateAccountTaskResult } from "../index.js";
import { harness } from "./helpers.js";

function rootManifest() {
  return {
    schema: 1, id: "tasks.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
    contract: { version: { major: 1, minor: 9 },
      capabilities: ["outbound-executor", "account-auth", "usage-reporter", "module-rpc"] },
    providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"],
      accountTasks: [{ id: "ping", action: "paid.ping" }] }],
    actions: [
      { id: "paid.ping", component: "paid", effect: "write", inputSchema: {} },
      { id: "normal.write", component: "paid", effect: "write", inputSchema: {} },
      { id: "normal.read", component: "paid", effect: "read", inputSchema: {} },
      { id: "plain.write", component: "plain", effect: "write", inputSchema: {} },
    ],
    services: [
      { id: "paid", runtime: "bun", entry: "worker.js", contract: { id: "paid", version: 1 },
        methods: ["paid.ping", "normal.write", "normal.read"], start: "lazy" },
      { id: "plain", runtime: "bun", entry: "worker.js", contract: { id: "plain", version: 1 },
        methods: ["plain.write"], start: "lazy" },
    ],
  };
}

const measured = {
  kind: "cache_keepalive", model: "m1",
  usage: { facts: { inputTokens: 17, outputTokens: 2, cachedTokens: 13, responseId: "native-response" } },
};

const configure = manifest => ({
  kind: "call", id: "configure", method: "component.configure", params: {
    component: manifest.component?.id ?? "primary", instance: "tasks",
    contract: manifest.component?.contract ?? {}, config: { version: "1", values: {} },
  },
});
const handlers = manifest => Object.fromEntries(requiredMethods(manifest)
  .filter(method => method !== "module.call").map(method => [method, async () => ({})]));

test("private paid service receives only exact task actions while ordinary services stay API1.1", () => {
  const root = rootManifest();
  const service = serviceManifest(root, "paid");
  assert.deepEqual(service.component.accountTaskActions, ["paid.ping"], "private paid action grant was lost");
  assert.deepEqual(service.contract, { version: { major: 1, minor: 9 }, capabilities: ["module-rpc"] });
  assert.deepEqual(requiredMethods(service), ["module.call"]);
  assert.equal(service.providers, undefined);
  assert.deepEqual(serviceManifest(root, "plain").contract,
    { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] });
  assert.equal(serviceManifest(root, "plain").component.accountTaskActions, undefined);
  const cloned = structuredClone(service);
  cloned.component.accountTaskActions[0] = "normal.write";
  assert.deepEqual(service.component.accountTaskActions, ["paid.ping"]);
  assert.throws(() => { publicManifest(service).component.accountTaskActions[0] = "normal.write"; }, TypeError);
  const rootCopy = structuredClone(root);
  rootCopy.providers[0].accountTasks[0].action = "normal.write";
  assert.deepEqual(serviceManifest(root, "paid").component.accountTaskActions, ["paid.ping"]);
  assert.deepEqual(publicManifest(root).providers[0].accountTasks, []);
  assert.deepEqual(publicManifest(root).actions, []);
});

test("private task grants reject oldminor, read, undeclared, duplicate and malformed actions", () => {
  const service = serviceManifest(rootManifest(), "paid");
  for (const change of [
    value => { value.contract.version.minor = 8; value.component.accountTaskActions = ["paid.ping"]; },
    value => { value.component.accountTaskActions = ["normal.read"]; },
    value => { value.component.accountTaskActions = ["missing"]; },
    value => { value.component.accountTaskActions = ["paid.ping", "paid.ping"]; },
    value => { value.component.accountTaskActions = "paid.ping"; },
  ]) {
    const invalid = structuredClone(service);
    change(invalid);
    assert.throws(() => validateManifest(invalid), "invalid private task action grant was admitted");
  }
});

test("private worker acknowledges task snapshots and loses reporting authority on cancellation", async () => {
  const manifest = serviceManifest(rootManifest(), "paid");
  let rejectedAfterCancel = false, handlerFinished;
  const finished = new Promise(resolve => { handlerFinished = resolve; });
  const actions = Object.fromEntries(manifest.actions.map(action => [action.id, async () => ({})]));
  actions["paid.ping"] = async (context, call, stream) => {
    await stream.emit("accepted", { operationId: call.scope.operation });
    await reportAccountTaskUsage(stream, measured);
    if (!context.signal.aborted)
      await new Promise(resolve => context.signal.addEventListener("abort", resolve, { once: true }));
    try {
      await reportAccountTaskUsage(stream, measured);
    } catch {
      rejectedAfterCancel = true;
    } finally {
      handlerFinished();
    }
    return { outcome: "uncertain" };
  };
  const worker = harness({ manifest, actions });
  try {
    worker.send(configure(manifest));
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "tick", method: "module.call",
      scope: { plugin: manifest.id, provider: "vendor", account: "selected", generation: 1,
        operation: "account-task-host-owned", attempt: 1 },
      params: { action: "paid.ping", input: { task: "ping", key: "conversation",
        job: "opaque-template", dueAt: 1794656400000, model: "m1" } } });
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    worker.send({ kind: "ack", id: accepted.id, seq: accepted.seq });
    const snapshot = await worker.received.next();
    assert.equal(snapshot.event, "accountTask.usage", "private paid worker dropped measured progress");
    assert.deepEqual(snapshot.data, measured);
    worker.send({ kind: "ack", id: snapshot.id, seq: snapshot.seq });
    worker.send({ kind: "cancel", id: "tick" });
    assert.equal((await worker.received.next()).kind, "result");
    await finished;
    assert.equal(rejectedAfterCancel, true);
  } finally {
    await worker.close();
  }
});

test("normal/read/undeclared actions cannot emit task progress on primary or private workers", async () => {
  for (const privateService of [false, true]) {
    for (const action of ["normal.write", "normal.read", "missing"]) {
      const root = rootManifest();
      if (!privateService) {
        root.actions = root.actions.filter(action => action.component === "paid")
          .map(({ component: _owner, ...action }) => action);
        delete root.services;
      }
      const manifest = privateService ? serviceManifest(root, "paid") : validateManifest(root);
      const worker = harness({ manifest, handlers: handlers(manifest),
        actions: Object.fromEntries(manifest.actions.map(action => [action.id,
          async (_context, _call, stream) => reportAccountTaskUsage(stream, measured)])) });
      try {
        worker.send(configure(manifest));
        assert.equal((await worker.received.next()).error, undefined);
        worker.send({ kind: "call", id: "ordinary", method: "module.call", params: { action, input: {} } });
        const reply = await worker.received.next();
        assert.equal(reply.kind, "result", "ordinary write gained paid task progress authority");
        assert.ok(reply.error);
      } finally {
        await worker.close();
      }
    }
  }
});

test("task-only private grant validates terminal facts without allowing own-call or provider authority", async () => {
  const manifest = serviceManifest(rootManifest(), "paid");
  assert.doesNotThrow(() => validateAccountTaskResult(manifest, { outcome: "completed", call: measured }));
  for (const call of [{ ...measured, usage: { facts: { inputTokens: -1 } } },
    { ...measured, account: "arbitrary" }, { ...measured, model: "not a model" }]) {
    assert.throws(() => validateAccountTaskResult(manifest, { outcome: "completed", call }));
  }
  const actions = Object.fromEntries(manifest.actions.map(action => [action.id, async () => ({})]));
  actions["paid.ping"] = async (_context, _call, stream) => appendOwnCall(stream, measured);
  const worker = harness({ manifest, actions });
  try {
    worker.send(configure(manifest));
    assert.equal((await worker.received.next()).error, undefined);
    for (const method of ["provider.usage", "provider.calls", "auth.refresh"]) {
      worker.send({ kind: "call", id: method, method });
      assert.equal((await worker.received.next()).error?.code, "method_not_found");
    }
    worker.send({ kind: "call", id: "append", method: "module.call", params: { action: "paid.ping", input: {} } });
    const reply = await worker.received.next();
    assert.equal(reply.kind, "result");
    assert.ok(reply.error, "private task grant gained usage.append authority");
  } finally {
    await worker.close();
  }
});
