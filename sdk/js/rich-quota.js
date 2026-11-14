import { SDKError } from "./bytes.js";

const invalid = () => { throw new SDKError("invalid_params", "Invalid rich quota metadata"); };

export function validateRichQuotaWindow(manifest, window) {
  if (manifest.contract.version.minor < 9) {
    if (["amount", "balance", "display", "unit", "span"].some(key => Object.hasOwn(window, key))) {
      throw new SDKError("invalid_params", "Rich quota metadata requires SDK API 1.9");
    }
    return;
  }
  for (const key of ["amount", "balance", "limit"]) {
    const value = window[key];
    if (value === undefined || key !== "limit" && value === null) continue;
    if (typeof value !== "number" || !Number.isFinite(value) || Math.abs(value) > Number.MAX_SAFE_INTEGER ||
      key !== "balance" && value < 0) invalid();
  }
  for (const [key, limit] of [["display", 512], ["unit", 64]]) {
    const value = window[key];
    if (value !== undefined && (typeof value !== "string" || new TextEncoder().encode(value).length > limit ||
      /[\p{Cc}\p{Cs}]/u.test(value))) invalid();
  }
  if (window.span !== undefined && (!Number.isInteger(window.span) || window.span < 0 || window.span > 9223372036)) invalid();
}
