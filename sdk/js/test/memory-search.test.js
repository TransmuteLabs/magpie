import assert from "node:assert/strict";
import { test } from "node:test";
import { SDKError, validateManifest } from "../index.js";
import { harness } from "./helpers.js";

const manifest = {
  schema: 1, id: "memory-fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 14 }, capabilities: ["tool-provider"] },
  compatibility: { hostAPI: "^1.14" },
  tools: [
    { name: "recall", exposure: "local-owner", memorySearch: true, inputSchema: {} },
    { name: "other", exposure: "local-owner", inputSchema: {} },
  ],
};

test("scoped Memory requires an opted-in local-owner tool and SDK 1.14", () => {
  assert.equal(validateManifest(manifest).tools[0].memorySearch, true);
  assert.throws(() => validateManifest({ ...manifest, compatibility: { hostAPI: "^1.15" } }), SDKError);
  assert.throws(() => validateManifest({ ...manifest, compatibility: undefined,
    contract: { ...manifest.contract, version: { major: 1, minor: 13 } } }), SDKError);
  for (const overrides of [{ exposure: "shared" }, { exposure: undefined }, { memorySearch: "yes" }]) {
    assert.throws(() => validateManifest({ ...manifest, tools: [{ ...manifest.tools[0], ...overrides }] }), SDKError);
  }
});

test("only the declared tool may ask the host to search; arguments cannot select scope", async () => {
  const worker = harness({
    manifest,
    handlers: { "tool.call": (_ctx, call, stream) => stream.hostCall("memory.search", call.params.arguments) },
  });
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", contract: {}, instance: "instance", config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    const call = (id, tool, args) => worker.send({ kind: "call", id, method: "tool.call",
      scope: { plugin: manifest.id }, params: { id: tool, arguments: args } });
    call("undeclared", "other", { query: "checkpoint" });
    assert.equal((await worker.received.next()).error?.code, "permission_denied");
    for (const args of [{ query: "checkpoint", project: "/elsewhere" }, { Query: "checkpoint" }, { query: "checkpoint", limit: null },
      { query: "特".repeat(400) }, { query: "checkpoint", limit: 11 }]) {
      call("invalid", "recall", args);
      assert.equal((await worker.received.next()).error?.code, "invalid_params");
    }
    call("valid", "recall", { query: "checkpoint", limit: 1 });
    const request = await worker.received.next();
    assert.equal(request.kind, "host.call");
    assert.equal(request.parent, "valid");
    assert.equal(request.method, "memory.search");
    assert.deepEqual(request.params, { query: "checkpoint", limit: 1 });
    const result = { hits: [{ kind: "fact", text: "checkpoint confirmed" }] };
    worker.send({ kind: "host.result", id: request.id, parent: request.parent, result });
    assert.deepEqual((await worker.received.next()).result, result);
  } finally {
    await worker.close();
  }
});
