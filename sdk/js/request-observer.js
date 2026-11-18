import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const count = value => Number.isSafeInteger(value) && value >= 0;
const text = value => typeof value === "string" && Buffer.byteLength(value) <= 600 && !/\p{Cc}/u.test(value);
const labels = ["agent", "kind", "provider", "model", "requested"];
const fields = ["phase", "requestId", "time", "attempt", "status", "millis", ...labels];

export function validateRequestObserver(manifest) {
  const declared = manifest.contract.capabilities.includes("request-observer");
  if (!declared && manifest.requestObserver == null) return;
  const descriptor = manifest.requestObserver;
  const timeout = descriptor?.timeoutMs ?? 0;
  if (!declared || !object(descriptor) || manifest.contract.version.minor < 12 || manifest.component != null
    || Object.keys(descriptor).some(key => key !== "timeoutMs")
    || !count(timeout) || timeout !== 0 && (timeout < 10 || timeout > 1000)) {
    throw new SDKError("invalid_params", "Invalid primary request observer declaration");
  }
}

export function validateRequestObservation(event) {
  const invalid = () => { throw new SDKError("invalid_params", "Invalid sanitized request observation"); };
  if (!object(event) || Object.keys(event).some(key => !fields.includes(key))
    || typeof event.requestId !== "string" || !/^[a-f0-9]{1,16}$/.test(event.requestId)
    || !count(event.time) || event.time === 0
    || ["attempt", "status", "millis"].some(key => !count(event[key] ?? 0)) || event.status > 599
    || labels.some(key => !text(event[key] ?? ""))) invalid();
  const { phase, provider, model, requested, status, millis } = event;
  const attempt = event.attempt ?? 0;
  if (phase === "start" && !attempt && !provider && !model && !requested && !status && !millis ||
    phase === "attempt" && attempt > 0 ||
    phase === "end" && !attempt && !provider && !requested) return event;
  invalid();
}

export function validateRequestObservationCall(call) {
  const scope = call.scope ?? {};
  if (call.auth !== undefined || ["provider", "account", "session", "operation", "generation", "attempt"].some(key => !!scope[key])) {
    throw new SDKError("permission_denied", "Request observation has no account, session or write authority");
  }
  validateRequestObservation(call.params);
}

export function validateRequestObservationResult(result) {
  if (result != null && (!object(result) || Object.keys(result).length)) {
    throw new SDKError("invalid_result", "Request observation cannot change the request or ledger");
  }
  return {};
}
