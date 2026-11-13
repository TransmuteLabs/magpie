import { Buffer } from "node:buffer";
import { SDKError } from "./errors.js";
export { SDKError } from "./errors.js";

export const MAX_FRAME_BYTES = 24 * 1024 * 1024;
export const MAX_CHUNK_BYTES = 64 * 1024;
export const MAX_HOST_BYTES = 1024 * 1024 + MAX_CHUNK_BYTES;
export const MAX_NATIVE_BODY_BYTES = 16 * 1024 * 1024;
export const MAX_CONCURRENT = 32;

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
