import assert from "node:assert/strict";
import test from "node:test";
import { requiredMethods, validateManifest, validateRequestObservation } from "../index.js";
import { harness } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "observer", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 12 }, capabilities: ["request-observer"] },
  requestObserver: {},
});
const start = { phase: "start", requestId: "abc", time: 1, agent: "pi" };

test("request observation is a separately negotiated primary capability", () => {
  assert.deepEqual(requiredMethods(validateManifest(manifest())), ["request.observe"]);
  for (const mutate of [
    m => { m.contract.version.minor = 11; },
    m => { m.contract.capabilities = ["operation-control"]; },
    m => { delete m.requestObserver; },
    m => { m.requestObserver.timeoutMs = 1001; },
    m => { m.requestObserver.bodies = true; },
  ]) {
    const value = manifest();
    mutate(value);
    assert.throws(() => validateManifest(value));
  }
});

test("request observation accepts phase-specific metadata", () => {
  assert.deepEqual(validateRequestObservation(start), start);
  for (const event of [
    { ...start, phase: "attempt", attempt: 1, status: 429, provider: "p", model: "m", millis: 4 },
    { ...start, phase: "end", status: 200, model: "p/m", millis: 10 },
  ]) {
    assert.deepEqual(validateRequestObservation(event), event);
  }
  for (const event of [
    { ...start, body: "synthetic" }, { ...start, phase: "attempt" },
    { ...start, phase: "end", provider: "p" }, { ...start, requestId: "a\n" },
    { ...start, time: 2 ** 53 }, { ...start, phase: "end", status: 600 },
  ]) {
    assert.throws(() => validateRequestObservation(event), { code: "invalid_params" });
  }
});

test("request observation RPC denies credentials, events, host callbacks and mutation", async () => {
  for (const mode of ["ordinary", "auth", "null auth", "scope", "event", "host", "mutation"]) {
    let called = 0;
    const worker = harness({ manifest: manifest(), handlers: {
      "request.observe": async (_ctx, _call, stream) => {
        called++;
        if (mode === "event") {
          try { await stream.emit("accepted", {}); } catch {}
        }
        if (mode === "host") {
          await assert.rejects(stream.hostCall("state.cas", {}), { code: "permission_denied" });
        }
        return mode === "mutation" ? { input: 99 } : {};
      },
    } });
    try {
      worker.send({ kind: "call", id: "configure", method: "component.configure", scope: {},
        params: { component: "primary", instance: "request-observer-instance", contract: {}, config: { version: "0", values: {} } } });
      assert.equal((await worker.received.next()).error, undefined);
      worker.send({ kind: "call", id: "observe", method: "request.observe", params: start,
        scope: { plugin: "observer", ...(mode === "scope" ? { session: "synthetic" } : {}) },
        ...(mode === "auth" ? { auth: { token: "synthetic" } } : {}),
        ...(mode === "null auth" ? { auth: null } : {}) });
      const frame = await worker.received.next();
      assert.equal(frame.kind, "result");
      assert.equal(!frame.error, ["ordinary", "host"].includes(mode), mode);
      assert.equal(called, ["auth", "null auth", "scope"].includes(mode) ? 0 : 1);
    } finally {
      await worker.close();
    }
  }
});
