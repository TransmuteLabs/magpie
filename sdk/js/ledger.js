import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const integer = value => Number.isSafeInteger(value) && value >= 0;
const amount = value => typeof value === "number" && Number.isFinite(value) && value >= 0;
const text = (value, limit) => typeof value === "string" && Buffer.byteLength(value) <= limit && !/[\p{Cc}]/u.test(value);
const invalid = () => { throw new SDKError("invalid_params", "Invalid sanitized ledger observation"); };

export function validateLedgerObserver(manifest) {
  const declared = manifest.contract.capabilities.includes("ledger-observer");
  if (!declared && manifest.ledger == null) return;
  if (!declared || !object(manifest.ledger) || manifest.contract.version.minor < 8 || manifest.component != null
    || Object.keys(manifest.ledger).some(key => key !== "timeoutMs")) invalid();
  const timeout = manifest.ledger.timeoutMs ?? 0;
  if (!Number.isSafeInteger(timeout) || timeout !== 0 && (timeout < 10 || timeout > 1000)) invalid();
}

export function validateLedgerObservation(row) {
  const counts = ["input", "output", "cacheRead", "cacheWrite", "cacheWrite1h", "reasoning", "millis", "ttft", "status", "tries"];
  const labels = ["operation", "agent", "provider", "model", "requested", "served", "errorType", "kind"];
  const booleans = ["cacheUnknown", "rejected", "handedOn"];
  const fields = ["time", ...counts, ...labels, ...booleans, "credits", "costUSD", "reportedCost"];
  if (!object(row) || Object.keys(row).some(key => !fields.includes(key)) || !integer(row.time) || row.time === 0
    || counts.some(key => !integer(row[key])) || row.status > 599 || row.cacheWrite1h > row.cacheWrite
    || labels.some(key => row[key] != null && !text(row[key], 600))
    || booleans.some(key => row[key] != null && typeof row[key] !== "boolean")
    || ["credits", "costUSD"].some(key => row[key] != null && !amount(row[key]))) invalid();
  if (row.reportedCost != null && (!object(row.reportedCost)
    || Object.keys(row.reportedCost).some(key => !["amount", "currency"].includes(key))
    || !amount(row.reportedCost.amount) || !text(row.reportedCost.currency, 16))) invalid();
  return row;
}

export function validateLedgerCall(call) {
  const scope = call.scope ?? {};
  if (call.auth != null || ["provider", "account", "session", "operation"].some(key => !!scope[key])
    || scope.generation || scope.attempt) {
    throw new SDKError("permission_denied", "Ledger observation has no account, session or write authority");
  }
  validateLedgerObservation(call.params);
}

export function validateLedgerResult(value) {
  if (value != null && (!object(value) || Object.keys(value).length)) invalid();
  return {};
}
