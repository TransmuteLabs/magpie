import { SDKError } from "./bytes.js";

export function validateProviderDefaults(manifest, provider) {
  for (const key of ["maxConcurrency", "maxAccounts"]) {
    const value = provider[key];
    if (value === undefined || value === null) continue;
    if (manifest.contract.version.minor < 9) throw new SDKError("invalid", "Provider defaults require SDK API 1.9");
    if (!Number.isInteger(value) || value < 0 || value > 1000) {
      throw new SDKError("invalid", "Provider defaults must be integers between 0 and 1000");
    }
  }
}
