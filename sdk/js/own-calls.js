import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";
import { validateExecutionUsage } from "./usage-facts.js";

export const MAX_OUTSIDE_CALLS = 500;
export const MAX_OUTSIDE_CALL_PAGES = 32;

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const text = (value, limit) => typeof value === "string" && Buffer.byteLength(value) <= limit && !/[\p{Cc}]/u.test(value);
const kind = value => typeof value === "string" && /^[a-z0-9_-]{1,64}$/.test(value);
const invalid = () => { throw new SDKError("invalid_params", "Invalid bounded account call"); };
const callFields = ["kind", "model", "status", "millis", "error", "usage"];
const counts = ["inputTokens", "outputTokens", "cachedTokens", "cacheWriteTokens", "cacheWrite1hTokens", "reasoningTokens"];

function fields(value, allowed) {
  if (!object(value) || Object.keys(value).some(key => !allowed.includes(key))) invalid();
}

function callFacts(manifest, call) {
  if (!text(call.model, 300) || !call.model || /\s/u.test(call.model)
    || call.kind != null && call.kind !== "" && !kind(call.kind)
    || call.status != null && (!Number.isSafeInteger(call.status) || call.status !== 0 && (call.status < 100 || call.status > 599))
    || call.millis != null && (!Number.isSafeInteger(call.millis) || call.millis < 0 || call.millis > 86400000)
    || call.error != null && !text(call.error, 1024)) invalid();
  if (call.usage != null) {
    fields(call.usage, [...counts.slice(0, 4), "cost", "currency", "facts"]);
    if (call.usage.facts != null) {
      fields(call.usage.facts, [...counts, "cacheUnknown", "deferred", "servedModel", "upstreamProvider",
        "requestId", "responseId", "stop", "tier", "errorType", "credits", "cost"]);
      if (call.usage.facts.deferred != null) fields(call.usage.facts.deferred, counts);
      if (call.usage.facts.cost != null) fields(call.usage.facts.cost, ["amount", "currency"]);
    }
  }
  validateExecutionUsage(manifest, call.usage);
  const facts = call.usage?.facts;
  if (facts?.deferred && counts.some(name => !Number.isSafeInteger((facts[name] ?? 0) + (facts.deferred[name] ?? 0)))) invalid();
}

export function validateOwnCall(manifest, request) {
  fields(request, ["call"]);
  fields(request.call, callFields);
  if (manifest.contract.version.minor < 8 || !manifest.contract.capabilities.includes("usage-reporter")
    || !kind(request.call.kind)) invalid();
  callFacts(manifest, request.call);
  return request;
}

export function validateOutsideCallsRequest(manifest, scope, request) {
  fields(request, ["since", "until", "cursor"]);
  if (manifest.contract.version.minor < 8 || !manifest.contract.capabilities.includes("usage-reporter")
    || !manifest.contract.capabilities.includes("account-auth") || scope.plugin !== manifest.id
    || !manifest.providers?.some(provider => provider.id === scope.provider && provider.calls === true)
    || !scope.account || !scope.generation || scope.session || scope.operation || scope.attempt
    || !Number.isSafeInteger(request.since) || request.since < 946684800000
    || !Number.isSafeInteger(request.until) || request.until < request.since
    || request.cursor != null && !text(request.cursor, 1024)) invalid();
  return request;
}

export function validateOutsideCallsResult(manifest, request, result) {
  fields(result, ["calls", "nextCursor"]);
  if (!Array.isArray(result.calls) || result.calls.length > MAX_OUTSIDE_CALLS
    || result.nextCursor != null && !text(result.nextCursor, 1024)
    || Buffer.byteLength(JSON.stringify(result)) > (1 << 20) + (64 << 10)) invalid();
  for (const call of result.calls) {
    fields(call, [...callFields, "id", "time", "session"]);
    if (!text(call.id, 300) || !call.id || !Number.isSafeInteger(call.time)
      || call.time < request.since || call.time > request.until
      || call.session != null && !text(call.session, 300)) invalid();
    callFacts(manifest, call);
  }
  return result;
}

export function appendOwnCall(stream, call) {
  return stream.hostCall("usage.append", { call });
}
