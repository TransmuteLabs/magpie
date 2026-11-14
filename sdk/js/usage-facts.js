import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const fields = ["inputTokens", "outputTokens", "cachedTokens", "cacheWriteTokens", "cacheWrite1hTokens", "reasoningTokens"];
const amount = value => typeof value === "number" && Number.isFinite(value) && value >= 0;
const text = (value, limit) => typeof value === "string" && new TextEncoder().encode(value).length <= limit && !/[\p{Cc}]/u.test(value);
const invalid = () => { throw new SDKError("invalid_params", "Invalid normalized execution usage"); };

function counts(value) {
  if (!object(value)) invalid();
  for (const field of fields) {
    if (value[field] != null && (!Number.isSafeInteger(value[field]) || value[field] < 0)) invalid();
  }
  if (value.cacheWrite1hTokens != null && (value.cacheWriteTokens == null
    || value.cacheWrite1hTokens > value.cacheWriteTokens)) invalid();
}

export function validateExecutionUsage(manifest, value) {
  if (value == null) return;
  if (!object(value)) invalid();
  for (const field of fields.slice(0, 4)) {
    if (value[field] != null && (!Number.isSafeInteger(value[field]) || value[field] < 0)) invalid();
  }
  if (value.cost != null && !amount(value.cost) || value.currency != null && !text(value.currency, 16)) invalid();
  const facts = value.facts;
  if (facts == null) return;
  if (manifest.contract.version.minor < 7 || fields.slice(0, 4).some(field => value[field])
    || value.cost || value.currency) invalid();
  counts(facts);
  if (facts.cacheUnknown != null && typeof facts.cacheUnknown !== "boolean") invalid();
  if (facts.deferred != null) counts(facts.deferred);
  for (const field of ["servedModel", "upstreamProvider", "requestId", "responseId", "stop", "tier", "errorType"]) {
    if (facts[field] != null && !text(facts[field], 300)) invalid();
  }
  if (facts.credits != null && !amount(facts.credits)) invalid();
  if (facts.cost != null && (!object(facts.cost) || !amount(facts.cost.amount)
    || !facts.cost.currency || !text(facts.cost.currency, 16))) invalid();
}
