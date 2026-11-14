import { Buffer } from "node:buffer";
import { MAX_HOST_BYTES, SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const text = (value, bytes) => typeof value === "string" && Buffer.byteLength(value) <= bytes && !/[\p{Cc}]/u.test(value);
const integer = value => Number.isSafeInteger(value) && value >= 0;
const invalid = () => { throw new SDKError("invalid_params", "Invalid catalog projection or access"); };

export function validateCatalogAccess(manifest) {
  const access = manifest.catalogAccess;
  if (access == null) return;
  if (manifest.contract.version.minor < 8 || !object(access) || Object.keys(access).some(key => key !== "scope")
    || !["selected", "inventory"].includes(access.scope)
    || access.scope === "selected" && manifest.component == null && !manifest.contract.capabilities.includes("account-auth")) invalid();
}

export function validateCatalogRead(manifest, scope, request) {
  validateCatalogAccess(manifest);
  if (!manifest.catalogAccess || !object(request) || Object.keys(request).length
    || manifest.catalogAccess.scope === "selected" && (!scope.provider || !scope.account || !scope.generation)) invalid();
}

export function validateCatalogProjection(value) {
  if (!object(value) || !Number.isSafeInteger(value.readAt) || value.readAt <= 0
    || !Array.isArray(value.accounts) || value.accounts.length > 256) invalid();
  const refs = new Set();
  let count = 0;
  for (const account of value.accounts) {
    if (!object(account) || !/^[a-f0-9]{64}$/.test(account.ref) || refs.has(account.ref)
      || !text(account.provider, 300) || !account.provider || !["account", "key", "public"].includes(account.kind)
      || !Array.isArray(account.models) || account.models.length > 4096 || (count += account.models.length) > 16384) invalid();
    refs.add(account.ref);
    const ids = new Set();
    for (const model of account.models) {
      if (!object(model) || !text(model.id, 600) || !model.id || ids.has(model.id)
        || model.name != null && !text(model.name, 300)
        || model.contextWindow != null && !integer(model.contextWindow)
        || model.maxOutput != null && !integer(model.maxOutput)) invalid();
      ids.add(model.id);
    }
    const quota = account.quota;
    if (!object(quota) || typeof quota.known !== "boolean" || quota.asOf != null && !integer(quota.asOf)
      || quota.windows != null && (!Array.isArray(quota.windows) || quota.windows.length > 1024 || !quota.known && quota.windows.length)) invalid();
    for (const window of quota.windows || []) {
      if (!object(window) || !text(window.name, 300) || !window.name
        || !Array.isArray(window.models) || !window.models.length || window.models.length > ids.size || new Set(window.models).size !== window.models.length
        || window.models.some(model => !ids.has(model))
        || window.used != null && (!Number.isFinite(window.used) || window.used < 0 || window.used > 100)
        || window.resetsAt != null && !integer(window.resetsAt)
        || ["aside", "unlimited", "credits"].some(key => window[key] != null && typeof window[key] !== "boolean")) invalid();
    }
  }
  if (Buffer.byteLength(JSON.stringify(value)) > MAX_HOST_BYTES) invalid();
}
