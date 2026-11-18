import { SDKError } from "./bytes.js";
import { validateRichQuotaWindow } from "./rich-quota.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const invalid = () => { throw new SDKError("invalid_params", "Invalid usage result or applicability"); };

export function parseUsageRevision(value) {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]*)$/.test(value) || value.length > 20 || BigInt(value) > 18446744073709551615n) invalid();
  return BigInt(value);
}

export function validateUsageResult(manifest, value) {
  if (!object(value) || value.windows !== undefined && (!Array.isArray(value.windows) || value.windows.length > 1024)) invalid();
  if (value.asOf !== undefined && (!Number.isSafeInteger(value.asOf) || value.asOf < 0)) invalid();
  if (Object.hasOwn(value, "revision")) {
    if (manifest.contract.version.minor < 4) invalid();
    parseUsageRevision(value.revision);
  }
  let fresh = !value.error && !value.asOf && !value.readDeferred, ordinary = false;
  for (const window of value.windows ?? []) {
    if (!object(window) || typeof window.name !== "string" || !window.name) invalid();
    validateRichQuotaWindow(manifest, window);
    if (manifest.contract.version.minor < 4 && ["models", "notModels", "aside"].some(key => Object.hasOwn(window, key))) invalid();
    if (window.aside !== undefined && typeof window.aside !== "boolean") invalid();
    for (const key of ["asOf", "resetsAt"]) {
      if (window[key] !== undefined && (!Number.isSafeInteger(window[key]) || window[key] < 0)) invalid();
    }
    for (const key of ["models", "notModels"]) {
      const models = window[key] ?? [];
      if (!Array.isArray(models) || models.length > 1024 || models.some(model =>
        typeof model !== "string" || !model || new TextEncoder().encode(model).length > 300 || /[\s\p{Cc}]/u.test(model))
        || new Set(models.map(model => model.toLowerCase())).size !== models.length) invalid();
    }
    if (window.models?.length && window.notModels?.length) invalid();
    const unknown = window.unknown || window.error || window.used === undefined || window.used === null;
    if (!unknown && (typeof window.used !== "number" || !Number.isFinite(window.used) || window.used < 0 || window.used > 100)) invalid();
    if (!window.aside) {
      ordinary = true;
      fresh = fresh && !unknown && !window.asOf;
    }
  }
  return Boolean(fresh && ordinary);
}
