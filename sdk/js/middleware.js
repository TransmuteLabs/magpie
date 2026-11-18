import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

export const MAX_MIDDLEWARE_BODY_BYTES = 1 << 20;
export const MIDDLEWARE_METHODS = Object.freeze([
  "middleware.request", "middleware.account", "middleware.provider", "middleware.event", "middleware.response",
]);
const flags = ["request", "account", "provider", "event", "response"];
const origins = ["local", "magpie-key", "gateway-key", "remote", "browser"];
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const safe = (value, limit) => typeof value === "string" && value.isWellFormed() &&
  Buffer.byteLength(value) <= limit && !/[\p{Cc}]/u.test(value);
const fail = message => { throw new SDKError("invalid_params", message); };
const fields = (value, keys) => object(value) && Object.keys(value).every(key => keys.includes(key));
const allows = (descriptor, origin) => (descriptor.origins?.length ? descriptor.origins : ["local", "magpie-key"]).includes(origin);

export function middlewareMethods(descriptor) {
  return flags.filter(flag => descriptor?.[flag]).map(flag => "middleware." + flag);
}

export function validateMiddleware(manifest) {
  const descriptor = manifest.middleware;
  const declared = manifest.contract.capabilities.includes("gateway-middleware");
  if (descriptor == null && !declared) return;
  if (!fields(descriptor, [...flags, "events", "origins"]) || !declared || manifest.contract.version.minor < 8 ||
    manifest.component != null || !flags.some(flag => descriptor[flag] === true) ||
    flags.some(flag => descriptor[flag] !== undefined && typeof descriptor[flag] !== "boolean")) {
    fail("Gateway middleware requires root API 1.8, gateway-middleware and declared hooks");
  }
  const events = descriptor.events ?? [], allowed = descriptor.origins ?? [];
  if (!Array.isArray(events) || events.length > 64 || events.length && !descriptor.event ||
    new Set(events).size !== events.length || events.some(event => !safe(event, 128)) ||
    !Array.isArray(allowed) || new Set(allowed).size !== allowed.length || allowed.some(origin => !origins.includes(origin))) {
    fail("Invalid middleware event filters or origins");
  }
}

function state(value, serialize) {
  if (value !== undefined && Buffer.byteLength(serialize(value)) > 65536) fail("Middleware state exceeds its byte limit");
}

export function validateMiddlewareCall(method, call, descriptor, serialize = JSON.stringify) {
  const scope = call.scope ?? {};
  if (scope.provider || scope.account || scope.session || scope.operation || scope.attempt ||
    scope.generation && scope.generation !== 0n || call.auth != null) fail("Middleware cannot borrow account, session or write authority");
  const value = call.params;
  if (!middlewareMethods(descriptor).includes(method) || !fields(value, ["body", "state", "context"])) fail("Invalid or undeclared middleware call");
  state(value.state, serialize);
  if (method === "middleware.account") {
    if (value.body !== undefined || value.context?.account == null) fail("Account hook requires only a sanitized account");
  } else if (!object(value.body) || Buffer.byteLength(serialize(value.body)) > MAX_MIDDLEWARE_BODY_BYTES) {
    fail("Middleware body must be a bounded JSON object");
  }
  const context = value.context;
  if (!fields(context, ["origin", "protocol", "model", "stream", "path", "agent", "session", "cwd", "query", "kind", "provider", "account", "event", "status"]) ||
    !origins.includes(context.origin) || !allows(descriptor, context.origin) ||
    ["protocol", "model", "path", "agent", "session", "cwd", "kind", "event"].some(key => context[key] !== undefined && !safe(context[key], 4096)) ||
    context.stream !== undefined && typeof context.stream !== "boolean" ||
    context.query !== undefined && (typeof context.query !== "string" || Buffer.byteLength(context.query) > 16384) ||
    context.status !== undefined && (!Number.isInteger(context.status) || context.status !== 0 && (context.status < 100 || context.status > 599))) {
    fail("Invalid middleware context or origin");
  }
  if (context.account != null) {
    const account = context.account;
    if (!fields(account, ["ref", "provider", "name", "kind", "model"]) || !/^[0-9a-f]{32}$/.test(account.ref) ||
      !safe(account.provider, 300) || !safe(account.name, 256) || !safe(account.kind, 128) || !safe(account.model, 4096)) {
      fail("Invalid sanitized middleware account");
    }
  }
  if (context.provider != null) {
    const provider = context.provider;
    if (!fields(provider, ["id", "name", "account", "model"]) || !safe(provider.id, 300) || !safe(provider.name, 256) ||
      !safe(provider.account ?? "", 128) || !safe(provider.model, 4096)) fail("Invalid middleware provider");
  }
  if (method === "middleware.provider" && context.provider == null) fail("Provider hook requires its current try");
  return value;
}

export function validateMiddlewareResult(method, value, serialize = JSON.stringify) {
  if (!MIDDLEWARE_METHODS.includes(method) || !fields(value, ["body", "state", "reject", "skip", "drop"]) ||
    value.body !== undefined && (!object(value.body) || Buffer.byteLength(serialize(value.body)) > MAX_MIDDLEWARE_BODY_BYTES) ||
    ["skip", "drop"].some(key => value[key] !== undefined && typeof value[key] !== "boolean")) fail("Invalid bounded middleware result");
  state(value.state, serialize);
  if (value.reject != null) {
    const reject = value.reject;
    if (method !== "middleware.request" || !fields(reject, ["status", "message"]) ||
      !Number.isInteger(reject.status) || reject.status < 400 || reject.status > 599 ||
      !safe(reject.message, 1024) || !reject.message.trim() || value.body !== undefined || value.skip || value.drop) {
      fail("Only request middleware may reject");
    }
  }
  if (value.skip && method !== "middleware.account" || value.drop && method !== "middleware.event" ||
    method === "middleware.account" && value.body !== undefined || value.drop && value.body !== undefined) {
    fail("Middleware result exceeds its hook authority");
  }
  return value;
}
