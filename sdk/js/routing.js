import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const control = /[\p{Cc}]/u;
const flags = ["exact", "byUse", "byFree", "byPlan", "bySpeed", "byPrice"];
const booleans = ["account", "known", "learns", "weekLow", "resting", "spent", "free", "freePlan", "credits", "first", "lead", "priced"];
const metrics = ["used", "lapsing", "pace", "tokens", "ttft", "tps", "left", "cost", "cacheCost"];
const fail = message => { throw new SDKError("invalid_params", message); };

export function validateRouting(manifest, reject) {
  const entries = manifest.routingStrategies;
  if (entries == null || Array.isArray(entries) && entries.length === 0) return;
  const total = ["inbounds", "providers", "agents", "sessions", "tools"].reduce((count, field) => count + (manifest[field]?.length || 0), 0);
  if (!Array.isArray(entries) || entries.length > 64 || total + entries.length > 1024 ||
    manifest.contract.version.minor < 4 || !manifest.contract.capabilities.includes("module-rpc") || manifest.component != null) {
    reject("Routing strategies require root SDK API 1.4 module-rpc within descriptor limits");
  }
  const seen = new Set();
  for (const descriptor of entries) {
    const action = manifest.actions?.find(candidate => candidate.id === descriptor?.action);
    if (!object(descriptor) || Object.keys(descriptor).some(key => !["id", "name", "action", "flags", "timeoutMs"].includes(key)) ||
      !id(descriptor.id) || seen.has(descriptor.id) || !id(descriptor.action) ||
      typeof descriptor.name !== "string" || !descriptor.name.trim() || Buffer.byteLength(descriptor.name) > 256 || control.test(descriptor.name) ||
      !action || action.effect !== "read" ||
      descriptor.timeoutMs !== undefined && descriptor.timeoutMs !== 0 &&
        (!Number.isInteger(descriptor.timeoutMs) || descriptor.timeoutMs < 25 || descriptor.timeoutMs > 2000)) {
      reject("Invalid routing strategy or undeclared read action");
    }
    if (descriptor.flags !== undefined && (!object(descriptor.flags) ||
      Object.keys(descriptor.flags).some(key => !flags.includes(key) || typeof descriptor.flags[key] !== "boolean"))) {
      reject("Invalid routing strategy flags");
    }
    seen.add(descriptor.id);
  }
}

export function validateRoutingRequest(request) {
  if (!object(request) || Object.keys(request).some(key => !["strategy", "pool", "seats"].includes(key)) ||
    !id(request.strategy) || typeof request.pool !== "string" || !request.pool || Buffer.byteLength(request.pool) > 512 || control.test(request.pool) ||
    !Array.isArray(request.seats) || !request.seats.length || request.seats.length > 256) fail("Invalid routing request bounds");
  const allowed = ["key", ...booleans, ...metrics, "soon", "unreached", "lapse", "weight", "leftIn"];
  for (const seat of request.seats) {
    if (!object(seat) || Object.keys(seat).some(key => !allowed.includes(key)) || !id(seat.key) ||
      !Number.isInteger(seat.weight) || seat.weight < 1 || seat.weight > 1000000 ||
      booleans.some(key => seat[key] !== undefined && typeof seat[key] !== "boolean") ||
      metrics.some(key => seat[key] !== undefined && (typeof seat[key] !== "number" || !Number.isFinite(seat[key]) || Math.abs(seat[key]) > 1e15)) ||
      seat.leftIn !== undefined && seat.leftIn !== "" && !id(seat.leftIn)) fail("Invalid routing seat bounds");
    const soon = seat.soon ?? [];
    const unreached = seat.unreached ?? 0;
    if (!Array.isArray(soon) || soon.length > 32 || soon.some(instant => !Number.isSafeInteger(instant)) ||
      !Number.isInteger(unreached) || unreached < 0 || unreached > soon.length ||
      seat.lapse !== undefined && !Number.isSafeInteger(seat.lapse)) fail("Invalid routing renewal bounds");
  }
  return request;
}

export function validateRoutingResult(result, seats) {
  if (!object(result) || Object.keys(result).some(key => key !== "order") ||
    !Number.isInteger(seats) || seats < 1 || seats > 256 || !Array.isArray(result.order) || result.order.length !== seats ||
    new Set(result.order).size !== seats || result.order.some(index => !Number.isInteger(index) || index < 0 || index >= seats)) {
    fail("Routing order must be an index permutation");
  }
  return result;
}
