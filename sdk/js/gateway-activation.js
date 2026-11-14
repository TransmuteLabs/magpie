import { SDKError } from "./bytes.js";

export function validateActivation(manifest) {
  const value = manifest.activation;
  if (value == null) return;
  if (!Array.isArray(value) || value.length > 1 || value.length === 1 && (value[0] !== "gateway"
    || manifest.contract.version.minor < 8 || manifest.component != null)) {
    throw new SDKError("invalid", "Activation requires SDK 1.8 and the finite root gateway declaration");
  }
}
