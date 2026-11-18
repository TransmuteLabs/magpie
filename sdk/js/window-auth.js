import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const id = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const fail = () => { throw new SDKError("invalid", "Invalid window authentication binding"); };

export function validateWindowAuthBindings(manifest) {
  if (manifest.authBindings === undefined) return;
  if (!Array.isArray(manifest.authBindings) || manifest.authBindings.length > 64) fail();
  if (!manifest.authBindings.length) return;
  if (manifest.contract.version.minor < 5 || !manifest.contract.capabilities.includes("account-auth")
    || !manifest.window || manifest.component != null) fail();
  const seen = new Set();
  for (const binding of manifest.authBindings) {
    if (!binding || typeof binding !== "object" || !id(binding.id) || seen.has(binding.id)
      || typeof binding.from !== "string" || !/^window:(?:page|panel:(?:0|[1-9][0-9]*))$/.test(binding.from)
      || binding.from.startsWith("window:panel:") && Number(binding.from.slice(13)) > 4294967295
      || !manifest.providers?.some(provider => provider.id === binding.provider)
      || !Array.isArray(binding.methods) || !binding.methods.length || binding.methods.length > 16
      || binding.reauthorize !== undefined && typeof binding.reauthorize !== "boolean") fail();
    const methods = new Set();
    for (const method of binding.methods) {
      if (!method || !id(method.id) || method.mode !== "poll" || methods.has(method.id)) fail();
      methods.add(method.id);
    }
    if (Buffer.byteLength(binding.from) > 256) fail();
    seen.add(binding.id);
  }
}
