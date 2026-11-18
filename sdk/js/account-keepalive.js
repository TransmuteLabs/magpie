import { SDKError } from "./bytes.js";

export function accountKeepAliveLead(policy) {
  if (policy == null) return 0;
  if (typeof policy !== "object" || Array.isArray(policy)
    || Object.keys(policy).some(key => key !== "leadMs")
    || Object.hasOwn(policy, "leadMs") && policy.leadMs !== undefined
      && (!Number.isInteger(policy.leadMs) || policy.leadMs < 60000 || policy.leadMs > 3600000)) {
    throw new SDKError("invalid", "Account keep-alive lead must be an integer from one minute through one hour");
  }
  return policy.leadMs ?? 60000;
}

export function validateAccountKeepAlives(manifest) {
  for (const provider of manifest.providers ?? []) {
    if (provider.keepAlive == null) continue;
    if (manifest.contract.version.minor < 8 || !manifest.contract.capabilities.includes("account-auth")
      || manifest.component != null) {
      throw new SDKError("invalid", "Account keep-alive requires SDK API 1.8 and account auth");
    }
    accountKeepAliveLead(provider.keepAlive);
  }
}
