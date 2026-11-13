import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

export function validateAccountSelection(value) {
  if (!value || typeof value !== "object" || Array.isArray(value) ||
    Object.keys(value).some(key => !["id", "expectedGeneration"].includes(key)) ||
    typeof value.id !== "string" || !value.id || Buffer.byteLength(value.id) > 256 || /[\x00-\x1f\x7f]/u.test(value.id) ||
    typeof value.expectedGeneration !== "string" || !/^[1-9][0-9]{0,19}$/.test(value.expectedGeneration) ||
    BigInt(value.expectedGeneration) > 18446744073709551615n) {
    throw new SDKError("invalid_params", "Account selection requires an identity and canonical nonzero uint64 generation");
  }
  return value;
}
