import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { encodeBytes, decodeBytes } from "../index.js";
import { stringifyJSON } from "../manifest.js";
import { inbox } from "./helpers.js";

export function bunRuntime() {
  const runtime = process.env.BUN_BIN || "bun";
  return spawnSync(runtime, ["--version"], { stdio: "ignore" }).status === 0 ? runtime : null;
}

export async function conformance(runtime) {
  const child = spawn(runtime, [fileURLToPath(new URL("./fixtures/worker.js", import.meta.url))], { stdio: ["pipe", "pipe", "pipe"] });
  const received = inbox(child.stdout);
  let stderr = "", finished = false;
  const closed = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => { finished = true; resolve({ code, signal }); });
  });
  closed.catch(() => {});
  child.stderr.on("data", bytes => { stderr += bytes.toString(); });
  const send = frame => child.stdin.write(stringifyJSON(frame.kind === "call" ? frame : { scope: {}, ...frame }) + "\n");
  const watchdog = setTimeout(() => child.kill("SIGKILL"), 15000);
  let calls = 0, events = 0;
  try {
    const invoke = async (method, params = null, extra = {}) => {
      const id = `call-${++calls}`;
      send({ kind: "call", id, method, params, scope: { plugin: "conformance", provider: "sample", account: "selected", generation: 9007199254740993n, operation: "operation", attempt: 1 }, ...extra });
      const streamed = [];
      while (true) {
        const frame = await received.next();
        if (frame.kind === "host.call") {
          assert.equal(frame.parent, id);
          assert.equal(frame.scope.account, "selected");
          assert.equal(frame.scope.generation, 9007199254740993n);
          assert.equal(frame.auth, undefined);
          if (frame.method === "account.replace") {
            assert.equal(streamed.at(-1)?.event, "accepted");
            assert.deepEqual(frame.params, { credentials: { token: "replacement-token" } });
            send({ kind: "host.result", id: frame.id, parent: id, result: { id: "selected", generation: 9007199254740994n } });
          } else if (frame.method === "network.proxy") {
            assert.deepEqual(Object.keys(frame.params), ["url"]);
            const url = frame.params.url.includes("direct.example.invalid") ? "" : "http://proxy.example.invalid:8080";
            send({ kind: "host.result", id: frame.id, parent: id, result: { url } });
          } else {
            assert.equal(frame.method, "state.get");
            send({ kind: "host.result", id: frame.id, parent: id, result: { found: true, version: "v1", value: 7 } });
          }
          continue;
        }
        assert.equal(frame.id, id);
        if (frame.kind === "event") {
          assert.equal(frame.seq, streamed.length + 1);
          streamed.push(frame);
          events++;
          send({ kind: "ack", id, seq: frame.seq });
          continue;
        }
        assert.equal(frame.kind, "result");
        return { frame, streamed };
      }
    };
    const described = await invoke("sdk.describe");
    assert.equal(described.frame.result.id, "conformance");
    assert.equal(described.frame.result.runtime, undefined);
    assert.equal(described.frame.result.contract.capabilities.length, 9);
    const body = encodeBytes(new TextEncoder().encode('{"model":"echo","messages":[]}'));
    assert.equal((await invoke("inbound.decode", { body })).frame.result.body, body);
    const started = await invoke("inbound.response", { action: "start" });
    assert.deepEqual(started.streamed.map(frame => frame.event), ["headers"]);
    assert.equal(started.streamed[0].data.status, 200);
    assert.equal(started.frame.result, null);
    assert.equal((await invoke("inbound.response", { action: "write", body })).streamed[0].data, body);
    const flushed = await invoke("inbound.response", { action: "flush" });
    assert.deepEqual(flushed.streamed.map(frame => frame.event), ["flush"]);
    assert.equal(flushed.frame.result, null);
    for (const action of ["finish", "cancel"]) {
      const terminal = await invoke("inbound.response", { action });
      assert.deepEqual(terminal.streamed, [], action + " emits no unsupported native event");
      assert.equal(terminal.frame.result, null);
    }
    const execution = await invoke("provider.execute", { protocol: "chat", model: "echo", body });
    assert.deepEqual(execution.streamed.map(frame => frame.event), ["accepted", "headers", "data", "usage"]);
    assert.deepEqual(decodeBytes(execution.streamed[2].data), decodeBytes(body));
    assert.equal(execution.frame.result.outcome, "completed");
    assert.equal((await invoke("provider.models")).frame.result.models[0].id, "echo");
    assert.equal((await invoke("provider.usage")).frame.result.windows[0].limit, 10);
    assert.equal((await invoke("auth.start")).frame.result.continuation.nonce, "fixture");
    assert.equal((await invoke("auth.finish")).frame.result.credentials.token, "fixture-token");
    assert.deepEqual((await invoke("auth.refresh", null, { auth: { token: "selected-token" } })).frame.result.credentials, { token: "selected-token" });
    assert.equal((await invoke("auth.logout")).frame.result, null);
    assert.equal((await invoke("operation.status", { mode: "host" })).frame.result.value, 7);
    assert.deepEqual((await invoke("operation.status", { mode: "proxy", url: "https://vendor.example.invalid/api" })).frame.result, { url: "http://proxy.example.invalid:8080" });
    assert.deepEqual((await invoke("operation.status", { mode: "proxy", url: "https://direct.example.invalid/api" })).frame.result, { url: "" });
    const replaced = await invoke("operation.status", { mode: "replace", credentials: { token: "replacement-token" } });
    assert.deepEqual(replaced.frame.result, { id: "selected", generation: 9007199254740994n });
    assert.deepEqual(replaced.streamed.map(frame => frame.event), ["accepted"]);
    assert.equal((await invoke("operation.cancel")).frame.result.outcome, "canceled");
    assert.deepEqual((await invoke("tool.call", { arguments: ["echo"] })).frame.result.content, ["echo"]);
    assert.equal((await invoke("agent.plan")).frame.result.files[0].body, "e30=");
    assert.equal((await invoke("session.convert", { to: "target", files: [{ path: "session.txt", format: "source", body }] })).frame.result.files[0].format, "target");
    assert.equal((await invoke("operation.status", { mode: "private-error" })).frame.error.code, "internal");
    assert.equal((await invoke("operation.status", { mode: "public-error" })).frame.error.outcome, "uncertain");
    send({ kind: "call", id: "cancel", method: "operation.status", params: { mode: "wait" } });
    const cancelEvent = await received.next();
    assert.equal(cancelEvent.kind, "event");
    send({ kind: "cancel", id: "cancel" });
    assert.equal((await received.next()).error.code, "canceled");
    send({ kind: "ack", id: "cancel", seq: cancelEvent.seq });
    assert.equal((await invoke("provider.models")).frame.result.models[0].id, "echo");
    send({ kind: "call", id: "cancel-host", method: "operation.status", params: { mode: "host-wait" }, scope: { plugin: "conformance" } });
    const canceledHost = await received.next();
    assert.equal(canceledHost.kind, "host.call");
    assert.equal(canceledHost.parent, "cancel-host");
    send({ kind: "cancel", id: "cancel-host" });
    assert.equal((await received.next()).error.code, "canceled");
    send({ kind: "host.result", id: canceledHost.id, parent: "cancel-host", result: { found: false } });
    assert.equal((await invoke("provider.models")).frame.result.models[0].id, "echo");
    child.stdin.end();
    assert.deepEqual(await closed, { code: 0, signal: null });
    assert.equal(stderr, "");
    return { calls: calls + 2, events: events + 1 };
  } finally {
    clearTimeout(watchdog);
    if (!finished) child.kill("SIGKILL");
    await closed;
    child.stdin.destroy();
    child.stdout.destroy();
    child.stderr.destroy();
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const runtimes = [process.execPath, bunRuntime()].filter(Boolean);
  for (const runtime of runtimes) {
    const result = await conformance(runtime);
    process.stdout.write(`${runtime}: ${result.calls} calls, ${result.events} acknowledged events passed\n`);
  }
}
