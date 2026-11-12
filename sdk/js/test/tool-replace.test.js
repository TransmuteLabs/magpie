import assert from "node:assert/strict";
import { test } from "node:test";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { bunRuntime } from "./conformance.js";
import { inbox } from "./helpers.js";

async function verifyToolReplacement(runtime) {
  const child = spawn(runtime, [fileURLToPath(new URL("./fixtures/worker.js", import.meta.url))], { stdio: ["pipe", "pipe", "pipe"] });
  const received = inbox(child.stdout);
  let finished = false, diagnostics = "";
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => { finished = true; resolve({ code, signal }); });
  });
  exited.catch(() => {});
  child.stderr.on("data", data => { diagnostics += data.toString(); });
  const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
  const send = frame => child.stdin.write(JSON.stringify(frame) + "\n");
  const scope = { plugin: "conformance", provider: "sample", account: "fixture-account", generation: 1, operation: "replace-operation", attempt: 1 };
  try {
    send({
      kind: "call", id: "tool-replace", method: "tool.call",
      params: { id: "echo", arguments: { replace: true } },
      scope, auth: { token: "fixture-token" },
    });
    const accepted = await received.next();
    assert.equal(accepted.kind, "event");
    assert.equal(accepted.id, "tool-replace");
    assert.equal(accepted.event, "accepted");
    assert.equal(accepted.seq, 1);
    assert.deepEqual(accepted.data, { operationId: scope.operation });
    send({ kind: "call", id: "before-ack", method: "sdk.describe", scope: { plugin: "conformance" } });
    const probe = await received.next();
    assert.equal(probe.kind, "result", "replacement waits while the host has not acknowledged acceptance");
    assert.equal(probe.id, "before-ack");
    assert.equal(probe.result.id, "conformance");
    send({ kind: "ack", id: "tool-replace", seq: accepted.seq, scope: {} });
    const replacement = await received.next();
    assert.equal(replacement.kind, "host.call");
    assert.equal(replacement.method, "account.replace");
    assert.equal(replacement.parent, "tool-replace");
    assert.notEqual(replacement.id, "tool-replace");
    assert.deepEqual(replacement.scope, scope);
    assert.deepEqual(replacement.params, { credentials: { token: "fixture-replaced" } });
    assert.equal(replacement.auth, undefined);
    const result = { id: "fixture-account", generation: 2 };
    send({ kind: "host.result", id: replacement.id, parent: "tool-replace", result, scope: {} });
    const terminal = await received.next();
    assert.equal(terminal.kind, "result");
    assert.equal(terminal.id, "tool-replace");
    assert.equal(terminal.error, undefined);
    assert.deepEqual(terminal.result, { content: result });
    child.stdin.end();
    assert.deepEqual(await exited, { code: 0, signal: null });
    assert.equal(diagnostics, "");
  } finally {
    clearTimeout(timer);
    if (!finished) child.kill("SIGKILL");
    await exited;
    child.stdin.destroy();
    child.stdout.destroy();
    child.stderr.destroy();
  }
}

test("real Node fixture tool replaces its bound account only after accepted ACK", { timeout: 15000 }, async () => {
  await verifyToolReplacement(process.execPath);
});

const bun = bunRuntime();
test("real Bun fixture tool replaces its bound account only after accepted ACK", { timeout: 15000, skip: !bun && "Bun is not installed" }, async () => {
  await verifyToolReplacement(bun);
});
