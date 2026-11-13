import assert from "node:assert/strict";
import { test } from "node:test";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { stringifyJSON } from "../manifest.js";
import { bunRuntime } from "./conformance.js";
import { inbox } from "./helpers.js";

async function verify(runtime) {
  const child = spawn(runtime, [fileURLToPath(new URL("./fixtures/gateway-worker.js", import.meta.url))], { stdio: ["pipe", "pipe", "pipe"] });
  const received = inbox(child.stdout), values = new Map();
  let diagnostics = "", finished = false, nextID = 0;
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => { finished = true; resolve({ code, signal }); });
  });
  exited.catch(() => {});
  child.stderr.on("data", data => { diagnostics += data.toString(); });
  const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
  const send = frame => child.stdin.write(stringifyJSON(frame) + "\n");
  const call = (id, method, params = {}, extra = {}) => send({ kind: "call", id, method, params, scope: { plugin: "hooks", session: "session", operation: id }, ...extra });
  const context = { origin: "local", protocol: "chat", model: "fixture", stream: false, path: "/v1/chat/completions", agent: "editor", session: "session", cwd: "/work", query: "", kind: "" };
  const invoke = async (method, params, extra = {}) => {
    const id = `call-${++nextID}`, events = [];
    call(id, method, params, extra);
    while (true) {
      const frame = await received.next();
      if (frame.kind === "host.call") {
        assert.equal(frame.parent, id);
        assert.equal(frame.scope.plugin, "hooks");
        assert.equal(frame.auth, undefined);
        assert.equal(frame.scope.provider, undefined);
        assert.equal(frame.scope.account, undefined);
        assert.equal(frame.scope.generation, undefined);
        if (frame.method === "state.cas") {
          assert.equal(events.at(-1)?.event, "accepted");
          values.set(frame.params.key, frame.params.value);
          send({ kind: "host.result", id: frame.id, parent: id, result: { swapped: true, version: "1" } });
        } else {
          assert.equal(frame.method, "state.get");
          send({ kind: "host.result", id: frame.id, parent: id, result: { found: true, version: "1", value: values.get(frame.params.key) } });
        }
      } else if (frame.kind === "event") {
        events.push(frame);
        assert.equal(frame.event, "accepted");
        send({ kind: "ack", id, seq: frame.seq });
      } else {
        assert.equal(frame.id, id);
        return { frame, events };
      }
    }
  };
  try {
    const described = (await invoke("sdk.describe", {})).frame.result;
    assert.deepEqual(described.contract.version, { major: 1, minor: 2 });
    assert.equal(described.tools[0].exposure, "local-owner");
    assert.equal(described.runtime, undefined);
    assert.equal((await invoke("gateway.request", { body: {}, context })).frame.error.code, "not_ready");
    assert.equal((await invoke("component.configure", { component: "primary", contract: {}, instance: "process-instance", config: { version: "1", values: {} } })).frame.error, undefined);
    assert.equal((await invoke("tool.call", { id: "remember", arguments: { fact: "offline fixture" } })).frame.error, undefined);
    const recalled = (await invoke("gateway.request", { body: { messages: [] }, context })).frame.result;
    assert.deepEqual(recalled, { body: { messages: [], memory: { fact: "offline fixture" } }, state: { session: "session" } });
    const captured = await invoke("gateway.response", { request: recalled.body, reply: {}, success: true, state: recalled.state, context });
    assert.deepEqual(captured.frame.result, { outcome: "domain-captured" });
    assert.deepEqual(values.get("capture"), { session: "session" });
    for (const [method, input] of [
      ["gateway.request", { body: {}, context: { ...context, origin: "browser" } }],
      ["gateway.response", { request: {}, reply: {}, success: true, state: "must not capture", context: { ...context, origin: "browser" } }],
    ]) {
      const refused = await invoke(method, input);
      assert.equal(refused.frame.error.code, "permission_denied");
      assert.deepEqual(refused.events, []);
    }
    assert.deepEqual(values.get("capture"), { session: "session" });
    const notification = { provider: "fixture", providerName: "Fixture", account: "Account", state: "ending", endsAt: "2026-11-13T10:00:00Z" };
    assert.equal((await invoke("gateway.signin", { event: notification })).frame.error, undefined);
    assert.deepEqual(values.get("notification"), notification);
    assert.equal((await invoke("gateway.request", { body: {}, context }, { auth: { token: "PRIVATE" } })).frame.error.code, "permission_denied");
    assert.equal((await invoke("gateway.request", { body: {}, context: { ...context, query: "stream" } })).frame.error.code, "invalid_params");
    const privateError = (await invoke("gateway.request", { body: {}, context: { ...context, query: "private-error" } })).frame;
    assert.equal(privateError.error.code, "internal");
    assert.equal(JSON.stringify(privateError).includes("PRIVATE"), false);
    call("cancel", "gateway.request", { body: {}, context: { ...context, query: "wait" } });
    const pending = await received.next();
    assert.equal(pending.kind, "host.call");
    send({ kind: "cancel", id: "cancel" });
    assert.equal((await received.next()).error.code, "canceled");
    send({ kind: "host.result", id: pending.id, parent: "cancel", result: null });
    assert.equal((await invoke("component.ready", {})).frame.result.instance, "process-instance");
    assert.equal((await invoke("component.stop", {})).frame.error, undefined);
    child.stdin.end();
    assert.deepEqual(await exited, { code: 0, signal: null });
    assert.equal(diagnostics, "");
  } finally {
    clearTimeout(timer);
    if (!finished) child.kill("SIGKILL");
    await exited;
    child.stdin.destroy(); child.stdout.destroy(); child.stderr.destroy();
  }
}

test("real Node API 1.2 hooks/tools preserve unary effects, private host state and cancellation", { timeout: 15000 }, async () => verify(process.execPath));
const bun = bunRuntime();
test("real Bun API 1.2 hooks/tools preserve unary effects, private host state and cancellation", { timeout: 15000, skip: !bun && "Bun is not installed" }, async () => verify(bun));
