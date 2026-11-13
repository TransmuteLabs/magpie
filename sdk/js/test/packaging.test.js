import assert from "node:assert/strict";
import { test } from "node:test";
import { CONTRACT, SDKError, publicManifest, requiredMethods, validateManifest } from "../index.js";
import { harness, manifest } from "./helpers.js";

function sharedManifest() {
  return {
    ...manifest(CONTRACT.capabilities.filter(capability => capability !== "module-rpc")),
    inbounds: [{ id: "native", path: "/fixture/native" }],
    providers: [{ id: "sample", name: "Sample", protocols: ["chat"], models: [{ id: "echo" }] }],
    agents: [{ id: "editor", name: "Editor" }],
    sessions: [{ id: "text", name: "Text" }],
    tools: [{ name: "echo", inputSchema: { type: "object" } }],
  };
}

test("native, bun and exec packaging have the same public nine-capability manifest", () => {
  const base = sharedManifest(), expected = publicManifest(validateManifest(base));
  for (const packaging of [
    { runtime: "exec", entry: "bin/worker", args: ["--stdio"] },
    { runtime: "bun", entry: "worker.js", args: ["--fixture"] },
    { runtime: "native", entry: "lib/worker.dylib" },
    { runtime: "native", entry: "lib/worker.so", args: [] },
    { runtime: "native", entry: "bin/worker.dll" },
  ]) {
    const frozen = validateManifest({ ...base, ...packaging });
    const published = publicManifest(frozen);
    assert.deepEqual(published, expected);
    assert.equal(published.contract.capabilities.length, 9);
    assert.deepEqual(requiredMethods(frozen), requiredMethods(base));
    for (const key of ["runtime", "entry", "args"]) assert.equal(Object.hasOwn(published, key), false);
  }
});

test("native packaging has no argv while existing bun and exec arguments remain valid", () => {
  for (const args of [undefined, []]) {
    assert.doesNotThrow(() => validateManifest({ ...manifest(), runtime: "native", entry: "lib/worker.so", args }));
  }
  for (const args of [["--stdio"], [""], ["--fixture", "value"], null, "argument", [1]]) {
    assert.throws(() => validateManifest({ ...manifest(), runtime: "native", entry: "lib/worker.so", args }), SDKError);
  }
  for (const runtime of ["bun", "exec"]) for (const args of [undefined, [], ["--stdio"], ["", "日本語"]]) {
    assert.doesNotThrow(() => validateManifest({ ...manifest(), runtime, args }));
  }
});

test("native library entry uses the same clean relative path limits as bun and exec", () => {
  for (const runtime of ["native", "bun", "exec"]) {
    assert.doesNotThrow(() => validateManifest({ ...manifest(), runtime, entry: "lib/worker.so" }));
    for (const entry of ["", ".", "..", "../worker.so", "/worker.so", "lib/../worker.so", "lib//worker.so",
      "lib/", "C:/worker.dll", "lib\\worker.dll", "lib/worker\0.so", "lib/worker\n.so", "x".repeat(1025)]) {
      assert.throws(() => validateManifest({ ...manifest(), runtime, entry }), SDKError, runtime + ": " + JSON.stringify(entry));
    }
    assert.doesNotThrow(() => validateManifest({ ...manifest(), runtime, entry: "x".repeat(1024) }));
  }
});

test("synthetic native metadata produces the same SDK describe handshake without loading a library", async () => {
  const base = sharedManifest(), expected = publicManifest(validateManifest(base));
  const handlers = Object.fromEntries(requiredMethods(base).map(method => [method, async () => null]));
  for (const runtime of ["native", "bun", "exec"]) {
    const source = { ...base, runtime, entry: runtime === "native" ? "lib/worker.so" : "worker.js" };
    const worker = harness({ manifest: source, handlers });
    try {
      worker.send({ kind: "call", id: "describe", method: "sdk.describe" });
      const frame = await worker.received.next();
      assert.equal(frame.kind, "result");
      assert.equal(frame.error, undefined);
      assert.deepEqual(frame.result, expected);
    } finally { await worker.close(); }
  }
});
