import assert from "node:assert/strict";
import { test } from "node:test";
import { requiredMethods, validateManifest, reportAccountTaskUsage } from "../index.js";
import { validateAccountTasks, validateAccountTaskSchedule, validateAccountTaskCancel,
  validateAccountTaskRequest, validateAccountTaskResult, scheduleAccountTask, cancelAccountTask } from "../account-tasks.js";
import { harness } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "tasks.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 9 }, capabilities: ["outbound-executor", "account-auth", "usage-reporter", "module-rpc"] },
  providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"], accountTasks: [{ id: "ping", action: "paid.ping" }] }],
  actions: [{ id: "paid.ping", effect: "write", inputSchema: { type: "object" } }],
});

test("paid task descriptors require API9 and declared writes with normal accounting", () => {
  const value = manifest();
  assert.doesNotThrow(() => validateAccountTasks(value));
  assert.doesNotThrow(() => validateManifest(value));
  for (const change of [
    target => { target.contract.version.minor = 8; },
    target => { target.contract.capabilities = target.contract.capabilities.filter(cap => cap !== "usage-reporter"); },
    target => { target.actions[0].effect = "read"; },
    target => { target.providers[0].accountTasks.push(target.providers[0].accountTasks[0]); },
    target => { target.providers[0].accountTasks[0].action = "missing"; },
  ]) {
    const broken = structuredClone(value);
    change(broken);
    assert.throws(() => validateAccountTasks(broken));
    assert.throws(() => validateManifest(broken));
  }
});

test("task requests cannot choose identities, model or unbounded timer payloads", () => {
  const request = { task: "ping", key: "conversation", job: "opaque-template", dueAt: 1794656400000 };
  assert.deepEqual(validateAccountTaskSchedule(request), request);
  for (const change of [{ dueAt: -1 }, { dueAt: Number.MAX_SAFE_INTEGER + 1 }, { dueAt: 1794656400000.5 }, { key: "" },
    { job: "" }, { job: "x".repeat(257) }, { account: "other" }, { model: "other" },
    { callerKeyId: "forged" }, { input: { vendorPayload: "forbidden" } }]) {
    assert.throws(() => validateAccountTaskSchedule({ ...request, ...change }));
  }
  assert.doesNotThrow(() => validateAccountTaskCancel({ task: "ping", key: "conversation" }));
  assert.throws(() => validateAccountTaskCancel({ task: "ping", key: "conversation", account: "other" }));
  assert.doesNotThrow(() => validateAccountTaskRequest({ ...request, model: "m1" }));
  assert.throws(() => validateAccountTaskRequest({ ...request, model: "m 1" }));
  assert.doesNotThrow(() => validateAccountTaskResult(manifest(), { outcome: "uncertain",
    call: { kind: "cache_keepalive", model: "m1", usage: { inputTokens: 7 } } }));
  for (const result of [null, {}, { outcome: "accepted" }, { outcome: "completed", account: "other" }])
    assert.throws(() => validateAccountTaskResult(manifest(), result));
});

test("actual worker task callbacks keep selected authority inside their live parent", async () => {
  const declared = manifest();
  const handlers = Object.fromEntries(requiredMethods(declared).map(method => [method, async () => ({})]));
  delete handlers["module.call"];
  handlers["provider.execute"] = async (_context, call, stream) => {
    await cancelAccountTask(stream, { task: "ping", key: "conversation" });
    await stream.emit("accepted", { operationId: call.scope.operation });
    return scheduleAccountTask(stream, { task: "ping", key: "conversation", job: "opaque-template", dueAt: 1794656400000 });
  };
  const worker = harness({ manifest: declared, handlers, actions: { "paid.ping": async () => ({ outcome: "not-started" }) } });
  const scope = { plugin: declared.id, provider: "vendor", account: "selected", generation: 1, operation: "foreground", attempt: 1 };
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure", params: {
      component: "primary", instance: "tasks", contract: {}, config: { version: "1", values: {} },
    } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "origin", method: "provider.execute", scope, auth: { token: "private" } });
    const canceled = await worker.received.next();
    assert.equal(canceled.method, "accountTasks.cancel");
    worker.send({ kind: "host.result", id: canceled.id, parent: canceled.parent, result: { canceled: true } });
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    worker.send({ kind: "ack", id: accepted.id, seq: accepted.seq });
    const scheduled = await worker.received.next();
    assert.equal(scheduled.method, "accountTasks.schedule");
    assert.deepEqual(scheduled.scope, scope);
    assert.equal(JSON.stringify(scheduled).includes("private"), false);
    worker.send({ kind: "host.result", id: scheduled.id, parent: scheduled.parent, result: { operationId: "account-task-host-owned" } });
    assert.deepEqual((await worker.received.next()).result, { operationId: "account-task-host-owned" });
  } finally {
    await worker.close();
  }
});

test("task usage snapshots are acknowledged events and a canceled stream cannot report again", async () => {
  const declared = manifest();
  const handlers = Object.fromEntries(requiredMethods(declared)
    .filter(method => method !== "module.call").map(method => [method, async () => ({})]));
  const measured = { kind: "cache_keepalive", model: "m1",
    usage: { facts: { inputTokens: 17, cachedTokens: 13, outputTokens: 2, responseId: "native-response" } } };
  let afterCancel;
  let finished;
  const handlerFinished = new Promise(resolve => { finished = resolve; });
  const worker = harness({ manifest: declared, handlers, actions: { "paid.ping": async (context, call, stream) => {
    await stream.emit("accepted", { operationId: call.scope.operation });
    await reportAccountTaskUsage(stream, measured);
    if (!context.signal.aborted)
      await new Promise(resolve => context.signal.addEventListener("abort", resolve, { once: true }));
    try {
      await reportAccountTaskUsage(stream, measured);
      afterCancel = "reported";
    } catch {
      afterCancel = "rejected";
    }
    finished();
    return { outcome: "uncertain" };
  } } });
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure", params: {
      component: "primary", instance: "tasks", contract: {}, config: { version: "1", values: {} },
    } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "tick", method: "module.call",
      scope: { plugin: declared.id, provider: "vendor", account: "selected", generation: 1,
        operation: "account-task-host-owned", attempt: 1 },
      params: { action: "paid.ping", input: { task: "ping", key: "conversation",
        job: "opaque-template", dueAt: 1794656400000, model: "m1" } } });
    const accepted = await worker.received.next();
    assert.equal(accepted.event, "accepted");
    worker.send({ kind: "ack", id: accepted.id, seq: accepted.seq });
    const snapshot = await worker.received.next();
    assert.equal(snapshot.event, "accountTask.usage");
    assert.deepEqual(snapshot.data, measured);
    worker.send({ kind: "ack", id: snapshot.id, seq: snapshot.seq });
    worker.send({ kind: "cancel", id: "tick" });
    assert.equal((await worker.received.next()).kind, "result");
    await handlerFinished;
    assert.equal(afterCancel, "rejected");
  } finally {
    await worker.close();
  }
});
