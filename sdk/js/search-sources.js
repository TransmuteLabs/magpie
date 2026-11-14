import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const text = (value, limit) => typeof value === "string" && value.isWellFormed() && Buffer.byteLength(value) <= limit;
const safe = (value, limit) => text(value, limit) && !/[\p{Cc}]/u.test(value);
const only = (value, keys) => object(value) && Object.keys(value).every(key => keys.includes(key));
const fail = message => { throw new SDKError("invalid_params", message); };

export function validateSearchSources(manifest, reject) {
  const entries = manifest.searchSources;
  if (entries == null || Array.isArray(entries) && entries.length === 0) return;
  const total = ["inbounds", "providers", "agents", "sessions", "tools", "routingStrategies"].reduce((count, field) => count + (manifest[field]?.length || 0), 0);
  if (!Array.isArray(entries) || entries.length > 64 || total + entries.length > 1024 ||
    manifest.contract.version.minor < 8 || !manifest.contract.capabilities.includes("module-rpc") || manifest.component != null) {
    reject("Search sources require root SDK API 1.8 module-rpc within descriptor limits");
  }
  const seen = new Set();
  for (const descriptor of entries) {
    const action = manifest.actions?.find(candidate => candidate.id === descriptor?.action);
    if (!only(descriptor, ["id", "name", "action", "provider"]) || !id(descriptor.id) || seen.has(descriptor.id) ||
      !safe(descriptor.name, 256) || !descriptor.name.trim() || !id(descriptor.action) ||
      !action || action.effect !== "read" || action.component && action.component !== "primary") {
      reject("Invalid search source or undeclared primary read action");
    }
    if (descriptor.provider !== undefined && descriptor.provider !== "" &&
      (!id(descriptor.provider) || !manifest.contract.capabilities.includes("account-auth") ||
        !manifest.contract.capabilities.includes("outbound-executor") || !manifest.providers?.some(provider => provider.id === descriptor.provider))) {
      reject("Search source requires its declared authenticated provider");
    }
    seen.add(descriptor.id);
  }
}

export function validateSearchRequest(request) {
  if (!only(request, ["source", "query", "count", "proxy"]) || !id(request.source) ||
    !text(request.query, 16384) || !request.query.trim() || request.query.includes("\0") ||
    !Number.isInteger(request.count) || request.count < 1 || request.count > 20 ||
    request.proxy !== undefined && !safe(request.proxy, 4096)) fail("Invalid search request bounds");
  return request;
}

export function validateSearchResult(result, count) {
  if (!only(result, ["pages", "status", "error", "retryAfter"]) || !Number.isInteger(count) || count < 1 || count > 20 ||
    !Array.isArray(result.pages) || result.pages.length > count ||
    !safe(result.error ?? "", 1024) || !safe(result.retryAfter ?? "", 128)) fail("Invalid search result bounds");
  const status = result.status ?? 0;
  if (status === 0 || status === 200) {
    if (result.error || result.retryAfter) fail("Successful search cannot contain a refusal");
  } else if (!Number.isInteger(status) || status < 400 || status > 599 || !result.error?.trim() || result.pages.length) {
    fail("Search refusal requires a status and error without pages");
  }
  for (const page of result.pages) {
    if (!only(page, ["title", "url", "text", "age"]) || !safe(page.title, 2048) || !safe(page.url, 4096) ||
      !text(page.text, 65536) || page.text.includes("\0") || !safe(page.age ?? "", 256)) fail("Invalid search page");
    let address;
    try { address = new URL(page.url); } catch { fail("Invalid search page URL"); }
    if (!["http:", "https:"].includes(address.protocol) || !address.host || address.username || address.password) fail("Invalid search page URL");
  }
  if (Buffer.byteLength(JSON.stringify(result)) > 1048576) fail("Search result exceeds its byte budget");
  return result;
}
