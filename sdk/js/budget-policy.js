import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const budgetMethods = ["budget.reserve", "budget.spent"];
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const fields = (value, allowed) => object(value) && Object.keys(value).every(key => allowed.includes(key));
const bounded = value => Buffer.byteLength(JSON.stringify(value) ?? "") <= 4096;
const integer = (value, min, max = Number.MAX_SAFE_INTEGER) => Number.isSafeInteger(value) && value >= min && value <= max;

const tokens = value => typeof value === "string" && /^(?:0|[1-9][0-9]{0,18})$/.test(value) && BigInt(value) <= 9223372036854775807n;

export function isBudgetMethod(method) {
  return budgetMethods.includes(method);
}

export function validateBudgetPolicies(manifest, validID) {
  const declared = manifest.contract.capabilities.includes("budget-policy");
  const policies = manifest.budgetPolicies ?? [];
  if (!declared && Array.isArray(policies) && !policies.length) return;
  if (!declared || manifest.contract.version.minor < 13 || manifest.component != null
    || !Array.isArray(policies) || !policies.length || policies.length > 32) {
    throw new SDKError("invalid_params", "Budget policies require a primary SDK API 1.13 declaration");
  }
  const seen = new Set();
  for (const p of policies) {
    if (!fields(p, ["id", "timeoutMs"]) || !validID(p.id) || seen.has(p.id)
      || p.timeoutMs != null && p.timeoutMs !== 0 && !integer(p.timeoutMs, 10, 1000)) {
      throw new SDKError("invalid_params", "Invalid budget policy descriptor");
    }
    seen.add(p.id);
  }
}

export function validateBudgetPolicyCall(manifest, call) {
  const scope = call.scope ?? {};
  if (call.auth !== undefined || ["provider", "account", "session", "operation", "generation", "attempt"].some(key => !!scope[key])) {
    throw new SDKError("permission_denied", "Budget policy has no selected or write scope");
  }
  const request = call.params;
  const reserve = call.method === "budget.reserve";
  if (!fields(request, ["policy", "time", "bodyBytes", "scopes"]) || !Array.isArray(request.scopes)
    || request.scopes.length > 10 || !integer(request.time, 1)
    || !manifest.budgetPolicies?.some(policy => policy.id === request.policy) || !isBudgetMethod(call.method)
    || (reserve ? !integer(request.bodyBytes, -1) : request.bodyBytes != null || !request.scopes.length)
    || request.scopes.some(limit => !fields(limit, ["owner", "scoped", "tokens", "costUSD"])
      || !["key", "issuer"].includes(limit.owner) || limit.scoped != null && typeof limit.scoped !== "boolean" || !reserve && !limit.scoped
      || !tokens(limit.tokens) || !Number.isFinite(limit.costUSD ?? 0) || limit.costUSD < 0 || limit.tokens === "0" && !limit.costUSD)
    || !bounded(request)) {
    throw new SDKError("invalid_params", "Invalid sanitized budget policy request");
  }
  return request;
}

export function validateBudgetPolicyResult(method, request, result) {
  const seen = new Set();
  if (!isBudgetMethod(method) || !fields(result, ["restrictions"]) || !Array.isArray(result.restrictions)
    || result.restrictions.length > request.scopes.length || method === "budget.reserve" && result.restrictions.length > 1
    || result.restrictions.some(entry => {
      if (!fields(entry, ["scope", "retrySeconds"]) || !integer(entry.scope, 1, request.scopes.length)
        || !integer(entry.retrySeconds, 1, 3600) || seen.has(entry.scope)) return true;
      seen.add(entry.scope);
      return false;
    }) || !bounded(result)) {
    throw new SDKError("invalid_result", "Invalid budget policy result");
  }
  return result;
}
