import { Buffer } from "node:buffer";

export const MAX_FRAME_BYTES = 24 * 1024 * 1024;
export const MAX_CHUNK_BYTES = 64 * 1024;
export const MAX_HOST_BYTES = 1024 * 1024 + MAX_CHUNK_BYTES;
export const MAX_NATIVE_BODY_BYTES = 16 * 1024 * 1024;
export const MAX_CONCURRENT = 32;

export class SDKError extends Error {
  constructor(code, message, outcome) {
    if (typeof code !== "string" || !/^[a-zA-Z0-9_.-]{1,64}$/.test(code)
      || typeof message !== "string" || !message.trim() || Buffer.byteLength(message) > 4096
      || /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]/.test(message)
      || outcome !== undefined && (typeof outcome !== "string" || !/^[a-zA-Z0-9_.-]{1,64}$/.test(outcome))) {
      throw new TypeError("Invalid public SDK error");
    }
    super(message);
    this.name = "SDKError";
    this.code = code;
    if (outcome !== undefined) this.outcome = outcome;
  }
}

export function encodeBytes(value, maxBytes = MAX_NATIVE_BODY_BYTES) {
  if (!(value instanceof Uint8Array) || !Number.isSafeInteger(maxBytes) || maxBytes < 0 || value.byteLength > maxBytes) {
    throw new SDKError("invalid", "Invalid or oversized binary data");
  }
  return Buffer.from(value.buffer, value.byteOffset, value.byteLength).toString("base64");
}

export function decodeBytes(value, maxBytes = MAX_NATIVE_BODY_BYTES) {
  if (typeof value !== "string" || !Number.isSafeInteger(maxBytes) || maxBytes < 0
    || value.length > 4 * Math.ceil(maxBytes / 3)
    || value.length % 4 !== 0 || /[^A-Za-z0-9+/=]/.test(value)
    || value.includes("=") && !/^[^=]*={1,2}$/.test(value)) {
    throw new SDKError("invalid", "Invalid or oversized base64 data");
  }
  const bytes = Buffer.from(value, "base64");
  if (bytes.byteLength > maxBytes || bytes.toString("base64") !== value) {
    throw new SDKError("invalid", "Invalid or oversized base64 data");
  }
  return new Uint8Array(bytes);
}
