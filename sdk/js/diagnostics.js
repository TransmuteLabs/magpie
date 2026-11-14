import { SDKError } from "./bytes.js";

const code = value => typeof value === "string" && /^[a-z][a-z0-9_.-]{0,63}$/.test(value);
const allowed = new Set(["level", "code", "phase", "status", "millis", "count", "retryable"]);
const invalid = () => { throw new SDKError("invalid_params", "Invalid bounded structured diagnostic"); };

export function validateDiagnostic(manifest, value) {
  if (manifest.contract.version.minor < 7 || !value || typeof value !== "object" || Array.isArray(value)
    || Object.keys(value).some(key => !allowed.has(key))
    || !["debug", "info", "warn", "error"].includes(value.level) || !code(value.code)) invalid();
  if (value.phase != null && value.phase !== "" && !code(value.phase)) invalid();
  if (value.status != null && (!Number.isSafeInteger(value.status)
    || value.status !== 0 && (value.status < 100 || value.status > 599))) invalid();
  for (const key of ["millis", "count"]) {
    if (value[key] != null && (!Number.isSafeInteger(value[key]) || value[key] < 0)) invalid();
  }
  if (value.millis > 24 * 60 * 60 * 1000 || value.retryable != null && typeof value.retryable !== "boolean") invalid();
}
