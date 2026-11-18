import assert from "node:assert/strict";
import test from "node:test";
import { requiredMethods, validateManifest, validateBudgetPolicyCall, validateBudgetPolicyResult } from "../index.js";
import { harness } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "budget", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 13 }, capabilities: ["budget-policy"] },
  budgetPolicies: [{ id: "extra" }],
});
const request = {
  policy: "extra", time: 10, bodyBytes: 123,
  scopes: [{ owner: "issuer", scoped: true, tokens: "9223372036854775807", costUSD: 0 }],
};
const reply = { restrictions: [{ scope: 1, retrySeconds: 30 }] };

test("budget policy is separately negotiated in API 1.13", () => {
  assert.deepEqual(requiredMethods(validateManifest(manifest())), ["budget.reserve", "budget.spent"]);
  for (const mutate of [
    m => { m.contract.version.minor = 12; },
    m => { m.budgetPolicies = []; },
    m => { m.budgetPolicies[0].timeoutMs = 1001; },
  ]) {
    const m = manifest();
    mutate(m);
    assert.throws(() => validateManifest(m));
  }
});

test("budget policy DTO accepts only declared host scope references", () => {
  assert.deepEqual(validateBudgetPolicyResult("budget.reserve", request, reply), reply);
  for (const bad of [
    { restrictions: [{ scope: 2, retrySeconds: 30 }] },
    { restrictions: [{ scope: 1, retrySeconds: 3601 }] },
    { ...reply, private: "body" },
    { Restrictions: [] },
    { restrictions: [{ Scope: 1, retrySeconds: 1 }] },
    { restrictions: [{ scope: 1, RetrySeconds: 1 }] },
    {},
  ]) {
    assert.throws(() => validateBudgetPolicyResult("budget.reserve", request, bad), { code: "invalid_result" });
  }
  for (const params of [
    { ...request, body: "private" },
    { ...request, policy: "outside" },
    { ...request, scopes: [{ ...request.scopes[0], tokens: "9223372036854775808" }] },
    { ...request, scopes: [{ ...request.scopes[0], key: "private" }] },
  ]) {
    assert.throws(() => validateBudgetPolicyCall(manifest(), { method: "budget.reserve", params }), { code: "invalid_params" });
  }
  assert.deepEqual(validateBudgetPolicyCall(manifest(), { method: "budget.reserve", params: request }), request);
});

test("budget policy worker grants no host authority and is unary", async () => {
  for (const mode of ["normal", "auth", "scope", "event", "host", "invalid"]) {
    let calls = 0;
    const handler = async (_ctx, _call, stream) => {
      calls++;
      if (mode === "host") await assert.rejects(stream.hostCall("state.cas", {}), { code: "permission_denied" });
      if (mode === "event") {
        try { await stream.emit("accepted", {}); } catch {}
      }
      return mode === "invalid" ? { restrictions: [{ scope: 2, retrySeconds: 20 }] } : reply;
    };
    const worker = harness({ manifest: manifest(), handlers: { "budget.reserve": handler, "budget.spent": handler } });
    try {
      worker.send({ kind: "call", id: "configuration", method: "component.configure", scope: {},
        params: { component: "primary", instance: "test", contract: {}, config: { version: "0", values: {} } } });
      assert.equal((await worker.received.next()).error, undefined);
      worker.send({ kind: "call", id: "reserve", method: "budget.reserve", params: request,
        scope: { plugin: "budget", ...(mode === "scope" ? { session: "private" } : {}) },
        ...(mode === "auth" ? { auth: { token: "private" } } : {}) });
      const result = await worker.received.next();
      assert.equal(result.kind, "result");
      assert.equal(!result.error, ["normal", "host"].includes(mode), mode);
      assert.equal(calls, ["auth", "scope"].includes(mode) ? 0 : 1);
    } finally {
      await worker.close();
    }
  }
});
