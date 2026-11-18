import assert from "node:assert/strict";
import test from "node:test";
import { validateManifest, requiredMethods, validateLedgerObservation } from "../index.js";
import { harness } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "ledger", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["ledger-observer"] },
  ledger: {},
});
const row = () => ({
  time: 1, agent: "agent", provider: "vendor", model: "model", input: 17, output: 3,
  cacheRead: 4, cacheWrite: 7, cacheWrite1h: 3, cacheUnknown: false,
  reasoning: 0, millis: 0, ttft: 0, status: 200, rejected: false, handedOn: false, tries: 1,
  credits: 0, reportedCost: { amount: 1, currency: "EUR" },
});

test("ledger permission is independently negotiated on the primary", () => {
  assert.deepEqual(requiredMethods(validateManifest(manifest())), ["ledger.observe"]);
  for (const mutate of [
    value => { value.contract.version.minor = 7; },
    value => { delete value.ledger; },
    value => { value.contract.capabilities = ["operation-control"]; },
    value => { value.ledger.timeoutMs = 1001; },
    value => { value.ledger.privateBody = true; },
  ]) {
    const value = manifest();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
});

test("ledger observations retain sparse currency facts and refuse private extensions", () => {
  assert.doesNotThrow(() => validateLedgerObservation(row()));
  const unspecified = row();
  unspecified.reportedCost.currency = "";
  assert.deepEqual(validateLedgerObservation(unspecified).reportedCost, { amount: 1, currency: "" });
  assert.equal(unspecified.costUSD, undefined);
  for (const mutate of [
    value => { value.time = 0; },
    value => { value.input = -1; },
    value => { value.input = 2 ** 53; },
    value => { value.cacheWrite1h = 8; },
    value => { value.status = 600; },
    value => { value.credits = NaN; },
    value => { value.body = "private"; },
    value => { value.reportedCost.currency = "EUR\n"; },
    value => { value.reportedCost.amount = -1; },
    value => { value.reportedCost.amount = Infinity; },
    value => { value.provider = "vendor\nprivate"; },
  ]) {
    const value = row();
    mutate(value);
    assert.throws(() => validateLedgerObservation(value));
  }
});

test("real ledger RPC is unary, has no credentials and cannot rewrite accounting", async () => {
  for (const mode of ["ordinary", "auth", "session", "operation", "event", "result"]) {
    let called = 0;
    const worker = harness({
      manifest: manifest(),
      handlers: { "ledger.observe": async (_ctx, _call, stream) => {
        called++;
        if (mode === "event") {
          try { await stream.emit("accepted", { operationId: "write" }); } catch {}
        }
        return mode === "result" ? { input: 99 } : {};
      } },
    });
    try {
      worker.send({ kind: "call", id: "configure", method: "component.configure", scope: {},
        params: { component: "primary", instance: "ledger-instance", contract: {}, config: { version: "0", values: {} } } });
      assert.equal((await worker.received.next()).error, undefined);
      worker.send({ kind: "call", id: "ledger", method: "ledger.observe", params: row(),
        scope: { plugin: "ledger", ...(mode === "session" ? { session: "private" } : {}),
          ...(mode === "operation" ? { operation: "write" } : {}) },
        ...(mode === "auth" ? { auth: { token: "private" } } : {}) });
      const result = await worker.received.next();
      assert.equal(result.kind, "result");
      assert.equal(!result.error, mode === "ordinary", mode);
      assert.equal(called, ["auth", "session", "operation"].includes(mode) ? 0 : 1);
    } finally {
      await worker.close();
    }
  }
});
