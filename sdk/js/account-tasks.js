import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";
import { validateOwnCallFacts } from "./own-calls.js";
import { actionOwner, componentID } from "./composite.js";

const fields = (value, allowed) => value !== null && typeof value === "object" && !Array.isArray(value)
  && Object.keys(value).every(key => allowed.includes(key));
const text = (value, limit) => typeof value === "string" && Buffer.byteLength(value) <= limit && !/[\p{Cc}]/u.test(value);
const id = value => typeof value === "string" && /^[a-z0-9_-]{1,64}$/.test(value);
const invalid = () => { throw new SDKError("invalid_params", "Invalid bounded selected-account task"); };
const dueTime = value => Number.isSafeInteger(value) && value >= 946684800000;

export function validateAccountTasks(manifest) {
  for (const provider of manifest.providers ?? []) {
    if (provider.accountTasks === undefined) continue;
    if (!Array.isArray(provider.accountTasks) || provider.accountTasks.length > 16) invalid();
    if (!provider.accountTasks.length) continue;
    if (manifest.contract.version.minor < 9 || manifest.component != null
      || ["account-auth", "outbound-executor", "module-rpc", "usage-reporter"].some(cap => !manifest.contract.capabilities.includes(cap))) invalid();
    const seen = new Set();
    for (const task of provider.accountTasks) {
      if (!fields(task, ["id", "action"]) || !id(task.id) || seen.has(task.id)
        || !manifest.actions?.some(action => action.id === task.action && action.effect === "write")) invalid();
      seen.add(task.id);
    }
  }
}

export function accountTaskUsageAllowed(manifest, id) {
  const action = manifest.actions?.find(action => action.id === id);
  if (manifest.contract.version.minor < 9 || !manifest.contract.capabilities.includes("module-rpc")
    || !action || action.effect !== "write" || actionOwner(action) !== componentID(manifest)) return false;
  if (manifest.component != null) return manifest.component.accountTaskActions?.includes(id) === true;
  return manifest.contract.capabilities.includes("usage-reporter")
    && manifest.providers?.some(provider => provider.accountTasks?.some(task => task.action === id)) === true;
}

export function validateAccountTaskUsage(manifest, call) {
  const allowed = manifest.component != null ? manifest.component.accountTaskActions?.length > 0
    : manifest.contract.capabilities.includes("usage-reporter");
  if (manifest.contract.version.minor < 9 || !allowed) invalid();
  return validateOwnCallFacts(manifest, call);
}

export function validateAccountTaskSchedule(value) {
  if (!fields(value, ["task", "key", "job", "dueAt"]) || !id(value.task) || !value.key || !text(value.key, 256)
    || !value.job || !text(value.job, 256) || !dueTime(value.dueAt)) invalid();
  return value;
}

export function validateAccountTaskCancel(value) {
  if (!fields(value, ["task", "key"]) || !id(value.task) || !value.key || !text(value.key, 256)) invalid();
  return value;
}

export function validateAccountTaskRequest(value) {
  if (!fields(value, ["task", "key", "job", "dueAt", "model", "proxy"]) || !id(value.task) || !value.key || !text(value.key, 256)
    || !value.job || !text(value.job, 256) || !dueTime(value.dueAt)
    || !value.model || !text(value.model, 300) || /\s/u.test(value.model)
    || value.proxy !== undefined && !text(value.proxy, 4096)) invalid();
  return value;
}

export function validateAccountTaskResult(manifest, value) {
  if (!fields(value, ["outcome", "call"]) || !["completed", "not-started", "uncertain"].includes(value.outcome)) invalid();
  if (value.call != null) validateAccountTaskUsage(manifest, value.call);
  return value;
}

export function scheduleAccountTask(stream, request) {
  return stream.hostCall("accountTasks.schedule", validateAccountTaskSchedule(request));
}

export function cancelAccountTask(stream, request) {
  return stream.hostCall("accountTasks.cancel", validateAccountTaskCancel(request));
}

export function reportAccountTaskUsage(stream, call) {
  return stream.emit("accountTask.usage", call);
}
