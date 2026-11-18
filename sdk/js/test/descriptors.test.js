import assert from "node:assert/strict";
import { test } from "node:test";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { bunRuntime } from "./conformance.js";
import { inbox } from "./helpers.js";

async function verifyDescriptors(runtime) {
  const child = spawn(runtime, [fileURLToPath(new URL("./fixtures/rich-worker.js", import.meta.url))], { stdio: ["pipe", "pipe", "pipe"] });
  const received = inbox(child.stdout);
  let finished = false, diagnostics = "", calls = 0;
  const exited = new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", (code, signal) => { finished = true; resolve({ code, signal }); });
  });
  exited.catch(() => {});
  child.stderr.on("data", data => { diagnostics += data.toString(); });
  const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
  const invoke = async (method, params = {}) => {
    const id = String(++calls);
    child.stdin.write(JSON.stringify({ kind: "call", id, method, params, scope: { plugin: "rich-conformance" } }) + "\n");
    const frame = await received.next();
    assert.equal(frame.kind, "result");
    assert.equal(frame.id, id);
    assert.equal(frame.error, undefined);
    return frame.result;
  };
  try {
    const manifest = await invoke("sdk.describe");
    assert.deepEqual(manifest.agents, [{
      id: "editor", name: "Editor", bin: "editor",
      configFiles: [".config/editor/settings.json"], fields: [{ key: "model", label: "Model" }],
    }]);
    assert.deepEqual(manifest.sessions, [{ id: "convert", name: "Convert", from: "source", to: "target", outputDir: ".config/converted" }]);
    assert.equal(manifest.runtime, undefined);
    assert.equal(manifest.entry, undefined);
    const inspected = await invoke("agent.inspect", { id: "editor", options: { model: "echo" }, settings: {} });
    assert.deepEqual(inspected, { connected: true, values: { model: "echo" }, detail: "Fixture only" });
    assert.deepEqual(await invoke("agent.inspect", { id: "editor" }), { connected: true, values: { model: "echo" }, detail: "Fixture only" });
    assert.deepEqual(await invoke("agent.plan", { id: "editor", action: "connect" }), { files: [] });
    const converted = await invoke("session.convert", {
      id: "convert", from: "source", to: "target",
      files: [{ path: "session.txt", format: "source", body: "Zml4dHVyZQ==" }],
    });
    assert.deepEqual(converted, { id: "converted-session", files: [{ path: "session.txt", format: "target", body: "Zml4dHVyZQ==" }] });
    assert.equal(calls, 5);
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

test("real Node child negotiates rich descriptors and inspect in five calls", { timeout: 15000 }, async () => {
  await verifyDescriptors(process.execPath);
});

const bun = bunRuntime();
test("real Bun child negotiates rich descriptors and inspect in five calls", { timeout: 15000, skip: !bun && "Bun is not installed" }, async () => {
  await verifyDescriptors(bun);
});
