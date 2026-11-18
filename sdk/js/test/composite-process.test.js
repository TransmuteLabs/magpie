import assert from "node:assert/strict";
import { test } from "node:test";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { stringifyJSON } from "../manifest.js";
import { bunRuntime } from "./conformance.js";
import { inbox } from "./helpers.js";

async function verify(runtime) {
  const child = spawn(runtime, [fileURLToPath(new URL("./fixtures/composite-worker.js", import.meta.url))], { stdio: ["pipe", "pipe", "pipe"] });
  const received = inbox(child.stdout);
  let diagnostics = "", finished = false;
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => { finished = true; resolve({ code, signal }); });
  });
  exited.catch(() => {});
  child.stderr.on("data", data => { diagnostics += data.toString(); });
  const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
  const send = frame => child.stdin.write(stringifyJSON(frame) + "\n");
  const call = (id, method, params = null, extra = {}) => send({ kind: "call", id, method, params, ...extra });
  try {
    call("describe", "sdk.describe");
    assert.deepEqual((await received.next()).result.contract.version, { major: 1, minor: 1 });
    call("early", "module.call", { action: "inspect", input: null });
    assert.equal((await received.next()).error.code, "not_ready");
    call("configure", "component.configure", {
      component: "primary", contract: {}, instance: "process-instance",
      config: { version: "18446744073709551615", values: { mode: "selected" } },
    });
    const ready = (await received.next()).result;
    assert.equal(ready.instance, "process-instance");
    assert.equal(ready.endpoints.ui, "http://127.0.0.1:8080/");
    call("inspect", "module.call", { action: "inspect", input: { value: 1 } }, { auth: { token: "selected-auth" } });
    const inspected = (await received.next()).result;
    assert.deepEqual(inspected.module.config.values, { mode: "selected" });
    assert.equal(inspected.module.config.version, "18446744073709551615");
    assert.equal(inspected.action, "inspect");
    assert.deepEqual(inspected.auth, { token: "selected-auth" });
    const scope = { plugin: "composite", provider: "provider", account: "account", generation: 18446744073709551615n, operation: "operation", attempt: 2 };
    call("write", "module.call", { action: "write", input: { value: 2 } }, { scope, auth: { token: "selected-auth" } });
    const accepted = await received.next();
    assert.equal(accepted.event, "accepted");
    assert.deepEqual(accepted.data, { operationId: "operation" });
    send({ kind: "ack", id: "write", seq: accepted.seq });
    const request = await received.next();
    assert.equal(request.method, "component.call");
    assert.equal(request.parent, "write");
    assert.deepEqual(request.scope, scope);
    assert.equal(request.auth, undefined);
    send({ kind: "host.result", id: request.id, parent: "write", result: { value: 3 } });
    assert.deepEqual((await received.next()).result, { value: 3 });
    call("stream", "module.call", { action: "stream", input: null });
    assert.equal((await received.next()).error.code, "invalid_params");
    call("stop", "component.stop");
    assert.deepEqual((await received.next()).result, {});
    call("after-stop", "component.ready");
    assert.equal((await received.next()).error.code, "canceled");
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

test("real Node SDK 1.1 process configures lifetime, actions, accepted ACK and component callback", { timeout: 15000 }, async () => verify(process.execPath));
const bun = bunRuntime();
test("real Bun SDK 1.1 process configures lifetime, actions, accepted ACK and component callback", { timeout: 15000, skip: !bun && "Bun is not installed" }, async () => verify(bun));
