import { SDKError } from "./bytes.js";

const namedStep = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);

function validPermit(request, result) {
  const facts = request.facts;
  const counts = [result.totalRetries ?? 0, result.providerRetries ?? 0, result.attempt ?? 0];
  return namedStep(request.step) && result.step === request.step && result.action === "wait"
    && !["unsafe", "revoked", "canceled", "budget-exhausted", "auth-recovery-unavailable",
      "not-retryable", "attempt-limit", "invalid-random", "delay-limit", "short-wait-exceeded",
      "selected-scope-refused", "budget-unavailable", "policy-unavailable", "step-limit",
      "no-eligible-account"].includes(result.reason)
    && !["auth-refusal", "request-refusal", "content-refusal", "unknown"].includes(result.class)
    && result.scope !== "unknown" && !(result.class === "quota" && result.scope === "request")
    && facts?.complete === true
    && ["provider.execute", "tool.call", "module.call"].includes(facts.operation)
    && facts.phase === "response" && Number.isInteger(facts.status)
    && facts.status >= 200 && facts.status <= 599 && !(facts.status >= 300 && facts.status < 400)
    && ![201, 202, 206].includes(facts.status)
    && ["read-only", "rejected-before-effect"].includes(request.evidence)
    && counts.every(value => Number.isSafeInteger(value) && value >= 0) && counts[1] <= counts[0];
}

// The host classifies refusals even when evidence is empty. It owns all retry
// limits and waiting; this helper never sleeps or refreshes credentials.
export async function decideRequest(signal, host, request) {
  if (!signal || typeof signal.throwIfAborted !== "function" || typeof host?.hostCall !== "function") {
    throw new SDKError("invalid_params", "Request decision requires its host and signal");
  }
  signal.throwIfAborted();
  const authority = { step: request?.step, evidence: request?.evidence, facts: { ...request?.facts } };
  const result = await host.hostCall("request.recover", request);
  signal.throwIfAborted();
  if (!result || typeof result !== "object" || Array.isArray(result)
    || result.retry != null && typeof result.retry !== "boolean"
    || result.retry === true && !validPermit(authority, result)) {
    throw new SDKError("invalid", "Host returned an invalid request retry permit");
  }
  return { ...result };
}

// classify returns null for success, otherwise bounded facts and declarative
// rules. It must preserve the response body when a retry is denied.
export async function requestWithPolicy(signal, host, step, request, transport, classify) {
  if (!signal || !host || typeof host.hostCall !== "function" || !namedStep(step) || !(request instanceof Request)
    || typeof transport !== "function" || typeof classify !== "function") {
    throw new SDKError("invalid_params", "Request policy requires its named request, host, transport and classifier");
  }
  const original = request.clone();
  for (;;) {
    signal.throwIfAborted();
    // manual also prohibits fetch's implicit cross-origin/downgrade forwarding.
    const response = await transport(original.clone(), { signal, redirect: "manual" });
    try {
      signal.throwIfAborted();
      if (!(response instanceof Response) || response.redirected
        || response.type !== "opaqueredirect" && (response.status < 200 || response.status > 599)) {
        throw new SDKError("invalid", "Request transport returned an invalid or redirected response");
      }
      if (response.type === "opaqueredirect") {
        return { response, decision: null };
      }
      const failure = await classify(response);
      signal.throwIfAborted();
      if (response.bodyUsed || response.body?.locked) {
        throw new SDKError("invalid", "Request classifier did not preserve the response body");
      }
      if (response.status >= 300 && response.status < 400) return { response, decision: null };
      if (!failure || !failure.facts?.operation) return { response, decision: null };
      const facts = { ...failure.facts, status: response.status };
      const decision = await decideRequest(signal, host, {
        ...failure, step, facts, evidence: facts.complete === true ? failure.evidence || "" : "",
      });
      if (response.bodyUsed || response.body?.locked) {
        throw new SDKError("invalid", "Request classifier did not preserve the response body");
      }
      if (!decision.retry) return { response, decision };
      await response.body?.cancel();
    } catch (error) {
      try { await response?.body?.cancel(); } catch { /* Preserve the original failure. */ }
      throw error;
    }
  }
}

// Final response authority is explicit. A previous callback classification is
// never promoted to an execution result by this helper.
export function refusedExecution(failure, windows = []) {
  return {status: failure.facts.status, outcome: "not-started", refusal: {
    step: failure.step, facts: failure.facts, rules: failure.rules,
    evidence: failure.evidence, windows,
  }};
}
