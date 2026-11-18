import assert from "node:assert/strict";
import { once } from "node:events";
import { createServer } from "node:http";
import { test } from "node:test";
import { decideRequest, requestWithPolicy } from "../index.js";

const signal = () => new AbortController().signal;
const failure = () => ({
  step: "named-read", evidence: "read-only", rules: [],
  facts: { status: 503, operation: "module.call", phase: "response", complete: true },
});
const permit = () => ({
  step: "named-read", action: "wait", retry: true,
  totalRetries: 2, providerRetries: 1, attempt: 1,
});

test("named retry permits reject contradictory well-typed replies", async context => {
  for (const [name, change] of Object.entries({
    "foreign-step": { step: "foreign-read" }, "empty-step": { step: "" },
    "terminal-action": { action: "terminal" }, "empty-action": { action: "" },
    "next-account": { action: "next-account" }, "unknown-action": { action: "retry" },
    "negative-total": { totalRetries: -1 }, "negative-provider": { providerRetries: -1 },
    "provider-over-root": { providerRetries: 3 }, "negative-attempt": { attempt: -1 },
    "known-unsafe": { reason: "unsafe" },
    "nonboolean-retry": { retry: "true" }, "fractional-total": { totalRetries: 1.5 },
    "infinite-total": { totalRetries: Infinity },
  })) {
    await context.test(name, async () => {
      let sends = 0, callbacks = 0, closes = 0;
      const host = { async hostCall() {
        callbacks++;
        return callbacks === 1 ? { ...permit(), ...change } : { ...permit(), action: "terminal", retry: false };
      } };
      await assert.rejects(requestWithPolicy(signal(), host, "named-read",
        new Request("https://fixture.invalid/read"), async () => {
          sends++;
          return new Response(new ReadableStream({ cancel() { closes++; } }), { status: 503 });
        }, failure));
      assert.equal(sends, 1);
      assert.equal(callbacks, 1);
      assert.equal(closes, 1);
    });
  }
});

test("decideRequest requires safe authority only when a repeat is granted", async context => {
  for (const [name, change] of Object.entries({
    "no-evidence": { evidence: "" }, "unknown-proof": { evidence: "idempotent" },
    "incomplete": { facts: { ...failure().facts, complete: false } },
    "wrong-phase": { facts: { ...failure().facts, phase: "dispatch" } },
    "no-operation": { facts: { ...failure().facts, operation: "" } },
    "unknown-operation": { facts: { ...failure().facts, operation: "arbitrary.call" } },
    "invalid-step": { step: "wrong step" }, "long-step": { step: "a".repeat(129) },
    "invalid-status": { facts: { ...failure().facts, status: 0 } },
    "redirect": { facts: { ...failure().facts, status: 307 } },
    "created": { facts: { ...failure().facts, status: 201 } },
    "accepted": { facts: { ...failure().facts, status: 202 } },
    "partial": { facts: { ...failure().facts, status: 206 } },
  })) {
    await context.test(name, async () => {
      const request = { ...failure(), ...change };
      await assert.rejects(decideRequest(signal(), { async hostCall() { return permit(); } }, request));
      const terminal = { action: "terminal", retry: false, reason: "unsafe", class: "unknown",
        scope: "request", totalRetries: 2, providerRetries: 1, nextAllowedAt: "2026-11-16T15:00:00Z" };
      assert.deepEqual(await decideRequest(signal(), { async hostCall() { return terminal; } }, request), terminal);
    });
  }
});

test("host callbacks cannot mutate named evidence into retry authority", async () => {
  const request = failure();
  request.evidence = "";
  await assert.rejects(decideRequest(signal(), { async hostCall(_method, sent) {
    sent.evidence = "read-only";
    return permit();
  } }, request));
});

test("redirects retain classifier capture and caller ownership without recovery", async () => {
  for (const status of [301, 303, 307, 308]) {
    let callbacks = 0, classified = 0, closes = 0;
    const host = { async hostCall() {
      callbacks++;
      return callbacks === 1 ? permit() : { ...permit(), action: "terminal", retry: false };
    } };
    const response = new Response(new ReadableStream({ cancel() { closes++; } }), { status });
    const result = await requestWithPolicy(signal(), host, "named-read",
      new Request("https://fixture.invalid/read"), async () => response, () => { classified++; return failure(); });
    assert.equal(result.response, response);
    assert.equal(result.decision, null);
    assert.equal(callbacks, 0);
    assert.equal(classified, 1);
    assert.equal(closes, 0);
    await response.body.cancel();
    assert.equal(closes, 1);
  }
});

test("a classifier must preserve the original response body", async () => {
  let callbacks = 0;
  const host = { async hostCall() { callbacks++; return { ...permit(), action: "terminal", retry: false }; } };
  await assert.rejects(requestWithPolicy(signal(), host, "named-read",
    new Request("https://fixture.invalid/read"), async () => new Response("original refusal", { status: 503 }),
    async response => { await response.text(); return failure(); }));
  assert.equal(callbacks, 0);
  const result = await requestWithPolicy(signal(), host, "named-read",
    new Request("https://fixture.invalid/read"), async () => new Response("original refusal", { status: 503 }),
    async response => { assert.equal(await response.clone().text(), "original refusal"); return failure(); });
  assert.equal(await result.response.text(), "original refusal");
  assert.equal(result.decision.totalRetries, 2);
});

test("cancellation during the host callback cannot retain a permit", async () => {
  const controller = new AbortController();
  await assert.rejects(decideRequest(controller.signal, { async hostCall() {
    controller.abort();
    return permit();
  } }, failure()));
});

test("invalid responses and locked classifier bodies never recover", async () => {
  let callbacks = 0;
  const host = { async hostCall() { callbacks++; return permit(); } };
  for (const response of [Response.error(), null, { status: 503 }]) {
    await assert.rejects(requestWithPolicy(signal(), host, "named-read",
      new Request("https://fixture.invalid/read"), async () => response, failure));
  }
  let reader;
  await assert.rejects(requestWithPolicy(signal(), host, "named-read",
    new Request("https://fixture.invalid/read"), async () => new Response("locked", { status: 503 }),
    response => { reader = response.body.getReader(); return failure(); }));
  await reader.cancel();
  reader.releaseLock();
  assert.equal(callbacks, 0);
});

test("known terminal contradictions never authorize another actual HTTP send", async context => {
  for (const [name, reason, classification, scope, valid] of [
    ["retry", "retry", "transient-rate", "account", true],
    ["future-reason", "future-host-diagnostic", "transient-outage", "request", true],
    ["optional-diagnostics", "", "", "", true],
    ["quota-wait", "retry", "quota", "model", true],
    ["unsafe", "unsafe", "transient-rate", "account", false],
    ["revoked", "revoked", "transient-rate", "account", false],
    ["canceled", "canceled", "transient-rate", "account", false],
    ["budget-exhausted", "budget-exhausted", "transient-rate", "account", false],
    ["auth-unavailable", "auth-recovery-unavailable", "transient-rate", "account", false],
    ["not-retryable", "not-retryable", "transient-rate", "account", false],
    ["attempt-limit", "attempt-limit", "transient-rate", "account", false],
    ["invalid-random", "invalid-random", "transient-rate", "account", false],
    ["delay-limit", "delay-limit", "transient-rate", "account", false],
    ["short-wait-exceeded", "short-wait-exceeded", "transient-rate", "account", false],
    ["selected-scope-refused", "selected-scope-refused", "transient-rate", "account", false],
    ["budget-unavailable", "budget-unavailable", "transient-rate", "account", false],
    ["policy-unavailable", "policy-unavailable", "transient-rate", "account", false],
    ["step-limit", "step-limit", "transient-rate", "account", false],
    ["no-eligible-account", "no-eligible-account", "transient-rate", "account", false],
    ["auth-class", "retry", "auth-refusal", "account", false],
    ["request-class", "retry", "request-refusal", "model", false],
    ["content-class", "retry", "content-refusal", "account", false],
    ["unknown-class", "retry", "unknown", "account", false],
    ["unknown-scope", "retry", "transient-rate", "unknown", false],
    ["request-quota", "retry", "quota", "request", false],
  ]) {
    await context.test(name, async () => {
      const grant = { ...permit(), reason, class: classification, scope };
      const host = { async hostCall() { return { ...grant }; } };
      if (valid) {
        assert.deepEqual(await decideRequest(signal(), host, failure()), grant);
      } else {
        await assert.rejects(decideRequest(signal(), host, failure()), { code: "invalid" });
      }
      const terminal = { ...grant, action: "terminal", retry: false };
      assert.deepEqual(await decideRequest(signal(), { async hostCall() { return terminal; } },
        { ...failure(), evidence: "" }), terminal);
      let sends = 0;
      const bodies = [];
      const server = createServer(async (request, response) => {
        let body = "";
        for await (const chunk of request) body += chunk;
        bodies.push(body);
        response.writeHead(++sends === 1 ? 503 : 200);
        response.end("bounded response");
      });
      server.listen(0, "127.0.0.1");
      await once(server, "listening");
      try {
        const repeat = requestWithPolicy(signal(), host, failure().step,
          new Request(`http://127.0.0.1:${server.address().port}/read`, { method: "POST", body: "same-request" }),
          fetch, response => response.status === 200 ? null : failure());
        if (valid) {
          const result = await repeat;
          assert.equal(await result.response.text(), "bounded response");
        } else {
          await assert.rejects(repeat, { code: "invalid" });
        }
        assert.equal(sends, valid ? 2 : 1);
        assert.ok(bodies.every(body => body === "same-request"));
      } finally {
        const closed = new Promise(resolve => server.close(resolve));
        server.closeAllConnections();
        await closed;
      }
    });
  }
});
