import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

export const MAX_GATEWAY_STATE_BYTES = 64 << 10;

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const origins = ["local", "magpie-key", "gateway-key", "remote", "browser"];
const controls = /[\p{Cc}]/u;
const space = /[\s\p{Cc}]/u;
const fail = message => { throw new SDKError("invalid", message); };
const invalid = message => { throw new SDKError("invalid_params", message); };

export function gatewayMethodEffect(method) {
  if (method === "gateway.request") return "read";
  if (method === "gateway.response" || method === "gateway.signin") return "write";
}

function allowsOrigin(descriptor, origin) {
  return (descriptor.origins?.length ? descriptor.origins : ["local", "magpie-key"]).includes(origin);
}

export function validateHooks(manifest) {
  for (const tool of manifest.tools ?? []) {
    if (tool.exposure == null || tool.exposure === "") continue;
    if (manifest.contract.version.minor < 2 || !["shared", "local-owner"].includes(tool.exposure)) {
      fail("Tool exposure requires SDK API 1.2 and shared or local-owner");
    }
  }
  const descriptor = manifest.hooks;
  const declared = manifest.contract.capabilities.includes("gateway-hooks");
  if (descriptor == null && !declared) return;
  if (manifest.contract.version.minor < 2 || !object(descriptor) || !declared) {
    fail("Gateway hooks require SDK API 1.2, gateway-hooks and a hooks descriptor");
  }
  if (manifest.component != null || !["request", "response", "signIn"].some(key => descriptor[key] === true)
    || ["request", "response", "signIn"].some(key => descriptor[key] != null && typeof descriptor[key] !== "boolean")
    || Object.keys(descriptor).some(key => !["request", "response", "signIn", "timeoutMs", "origins"].includes(key))) {
    fail("Gateway hooks require valid declared hooks on the primary component");
  }
  const timeout = descriptor.timeoutMs ?? 0;
  if (!Number.isInteger(timeout) || timeout !== 0 && (timeout < 100 || timeout > 30000)) {
    fail("Gateway hook timeout must be 100..30000 milliseconds or zero");
  }
  const allowed = descriptor.origins ?? [];
  if (!Array.isArray(allowed) || new Set(allowed).size !== allowed.length || allowed.some(origin => !origins.includes(origin))) {
    fail("Unknown or duplicate gateway hook origin");
  }
  if (descriptor.signIn && !allowsOrigin(descriptor, "local")) fail("Sign-in hooks require local origin permission");
}

function fields(value, allowed) {
  if (!object(value) || Object.keys(value).some(key => !allowed.includes(key))) invalid("Invalid gateway hook parameters");
}

function state(value, serialize) {
  if (value !== undefined && Buffer.byteLength(serialize(value)) > MAX_GATEWAY_STATE_BYTES) {
    throw new SDKError("resource_exhausted", "Gateway hook state exceeds its byte limit");
  }
}

function context(descriptor, value) {
  fields(value, ["origin", "protocol", "model", "stream", "path", "agent", "session", "cwd", "query", "kind"]);
  if (Object.entries(value).some(([key, value]) => value != null && typeof value !== (key === "stream" ? "boolean" : "string"))
    || !origins.includes(value.origin)) invalid("Invalid gateway request context or origin");
  if (!allowsOrigin(descriptor, value.origin)) throw new SDKError("permission_denied", "Gateway origin is outside this hook's declaration");
}

// Check calendar dates without coercing RFC3339 text or its fractional precision.
function timestamp(value) {
  const match = /^(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:[.,]\d+)?(?:Z|[+-](\d\d):(\d\d))$/.exec(value);
  if (!match) return false;
  const [year, month, day, hour, minute, second] = match.slice(1, 7).map(Number);
  const offsetHour = Number(match[7] ?? 0), offsetMinute = Number(match[8] ?? 0);
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return month >= 1 && month <= 12 && day >= 1 && day <= days[month - 1]
    && hour <= 23 && minute <= 59 && second <= 59 && offsetHour <= 24 && offsetMinute <= 60;
}

export function validateGatewayCall(method, call, descriptor, serialize) {
  const scope = call.scope;
  if (scope.provider || scope.account || scope.generation !== undefined && scope.generation !== 0 && scope.generation !== 0n
    || call.auth != null) {
    throw new SDKError("permission_denied", "Gateway hooks cannot receive provider, account or auth scope");
  }
  const value = call.params;
  if (method === "gateway.request") {
    fields(value, ["body", "context"]);
    if (!object(value.body)) invalid("Gateway request body must be a JSON object");
    context(descriptor, value.context);
  } else if (method === "gateway.response") {
    fields(value, ["request", "reply", "success", "state", "context"]);
    if (!object(value.request) || value.success != null && typeof value.success !== "boolean"
      || value.success && !object(value.reply)) invalid("Invalid completed gateway exchange");
    state(value.state, serialize);
    context(descriptor, value.context);
  } else {
    fields(value, ["event"]);
    const event = value.event;
    fields(event, ["provider", "providerName", "account", "state", "endsAt"]);
    if (typeof event.provider !== "string" || !event.provider || Buffer.byteLength(event.provider) > 300 || space.test(event.provider)
      || ["providerName", "account", "endsAt"].some(key => event[key] != null && typeof event[key] !== "string")
      || Buffer.byteLength(event.providerName ?? "") > 256 || Buffer.byteLength(event.account ?? "") > 256
      || controls.test((event.providerName ?? "") + (event.account ?? ""))
      || !["signed-out", "ending"].includes(event.state)) invalid("Invalid sanitized gateway sign-in event");
    if (event.endsAt && !timestamp(event.endsAt)) invalid("Sign-in expiry must be an RFC3339 timestamp");
  }
}

export function validateGatewayResult(value, serialize) {
  fields(value, ["body", "state"]);
  if (value.body !== undefined && !object(value.body)) invalid("Gateway request result requires optional object body and JSON state");
  state(value.state, serialize);
  return value;
}
