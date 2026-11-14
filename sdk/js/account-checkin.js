import { SDKError } from "./bytes.js";

const object = value => value != null && typeof value === "object" && !Array.isArray(value);
const fail = message => { throw new SDKError("invalid", message); };
const day = value => typeof value === "string" && /^\d{4}-\d{2}-\d{2}$/.test(value)
  && Number.isFinite(Date.parse(value)) && new Date(value).toISOString().slice(0, 10) === value;
const text = (value, multiline = false) => typeof value === "string" && Buffer.byteLength(value) <= 4096
  && !/\p{Cc}/u.test(multiline ? value.replace(/[\n\t]/g, "") : value) && !/[\uD800-\uDFFF]/u.test(value);

export function validateAccountCheckins(manifest) {
  for (const provider of manifest.providers ?? []) {
    const value = provider.checkin;
    if (value == null) continue;
    const action = manifest.actions?.find(action => action.id === value.action);
    if (!object(value) || Object.keys(value).some(key => !["action", "recheck"].includes(key))
      || value.recheck != null && typeof value.recheck !== "boolean" || manifest.contract.version.minor < 8
      || !manifest.contract.capabilities.includes("account-auth") || !manifest.contract.capabilities.includes("module-rpc")
      || !action || action.effect !== "write" || manifest.component != null) {
      fail("Account check-in requires SDK 1.8, account auth and a declared module write");
    }
  }
}

export function validateAccountCheckinRequest(value) {
  if (!object(value) || Object.keys(value).some(key => !["day", "proxy"].includes(key))
    || !day(value.day)
    || value.proxy != null && !text(value.proxy)) fail("Invalid account check-in day or selected proxy");
  return structuredClone(value);
}

export function validateAccountCheckinResult(value) {
  if (!object(value) || Object.keys(value).some(key => !["outcome", "credit", "streak", "message", "until"].includes(key))
    || !["claimed", "done", "inactive", "ineligible", "failed", "captcha", "own-app"].includes(value.outcome)
    || value.credit != null && (typeof value.credit !== "number" || !Number.isFinite(value.credit) || value.credit < 0)
    || value.streak != null && (!Number.isInteger(value.streak) || value.streak < 0 || value.streak > 2147483647)
    || value.message != null && !text(value.message, true)
    || value.until != null && value.until !== "" && (typeof value.until !== "string" || value.until.length > 64
      || !/^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.\d{1,9})?(?:Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$/.test(value.until)
      || !day(value.until.slice(0, 10))
      || !Number.isFinite(Date.parse(value.until)))) fail("Invalid account check-in result");
  return structuredClone(value);
}
