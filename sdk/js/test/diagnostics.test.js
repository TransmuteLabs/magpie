import assert from "node:assert/strict";
import { test } from "node:test";
import { validateDiagnostic } from "../index.js";
import { harness, moduleFor } from "./helpers.js";

const manifest = { contract: { version: { minor: 7 } } };

test("structured diagnostics admit only negotiated bounded non-secret facts", () => {
  const diagnostic = { level: "warn", code: "vendor_timeout", phase: "provider.execute", status: 504, millis: 1200, count: 2 };
  assert.doesNotThrow(() => validateDiagnostic(manifest, diagnostic));
  for (const change of [
    { level: "fatal" }, { code: "forged\nentry" }, { code: "x".repeat(65) },
    { phase: "https://vendor.invalid?token=secret" }, { status: 600 }, { millis: -1 },
    { millis: 24 * 60 * 60 * 1000 + 1 }, { count: -1 }, { retryable: 1 },
    { plugin: "forged" }, { message: "Authorization: Bearer private" }, { body: "private prompt" },
  ]) assert.throws(() => validateDiagnostic(manifest, { ...diagnostic, ...change }));
  assert.throws(() => validateDiagnostic({ contract: { version: { minor: 6 } } }, diagnostic));
});

test("structured diagnostic worker callback carries the live scope without auth", async () => {
  const module = moduleFor(async (_context, _call, stream) => stream.hostCall("diagnostics.emit", {
    level: "warn", code: "vendor_timeout", status: 504,
  }));
  module.manifest.contract.version.minor = 7;
  const worker = harness(module);
  try {
    worker.send({
      kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "1", values: {} } },
    });
    assert.equal((await worker.received.next()).error, undefined);
    const scope = { plugin: "fixture", provider: "vendor", account: "selected", generation: 5, operation: "operation" };
    worker.send({ kind: "call", id: "parent", method: "operation.status", scope, auth: { token: "private-credential" } });
    const callback = await worker.received.next();
    assert.equal(callback.kind, "host.call");
    assert.equal(callback.method, "diagnostics.emit");
    assert.deepEqual(callback.scope, scope);
    assert.equal(callback.auth, undefined);
    assert.equal(JSON.stringify(callback.params).includes("private-credential"), false);
    worker.send({ kind: "host.result", id: callback.id, parent: callback.parent, result: { recorded: true } });
    assert.deepEqual((await worker.received.next()).result, { recorded: true });
  } finally { await worker.close(); }
});
