import { Buffer } from "node:buffer";
import { SDKError } from "./bytes.js";

const id = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const word = value => typeof value === "string" && !!value.trim() && Buffer.byteLength(value) <= 256 && !/[\p{Cc}]/u.test(value);
const fail = () => { throw new SDKError("invalid", "Invalid window authentication binding"); };

function validateFields(fields) {
  if (fields === undefined) return;
  if (!Array.isArray(fields) || fields.length > 16) fail();
  const keys = new Set();
  for (const field of fields) {
    if (!field || typeof field.key !== "string" || !/^[a-z][a-zA-Z0-9_-]{0,39}$/.test(field.key)
      || ["constructor", "prototype"].includes(field.key) || keys.has(field.key) || !word(field.label)
      || !["text", "secret", "select"].includes(field.type)
      || field.optional !== undefined && typeof field.optional !== "boolean") fail();
    const choices = field.choices === undefined ? [] : field.choices;
    if (!Array.isArray(choices) || (field.type === "select" ? !choices.length || choices.length > 32 : choices.length !== 0)
      || choices.some(choice => !choice || !word(choice.value) || !word(choice.label))
      || new Set(choices.map(choice => choice.value)).size !== choices.length) fail();
    keys.add(field.key);
  }
}

export function validateWindowAuthBindings(manifest) {
  const signIn = manifest.contract.capabilities.includes("provider-signin");
  const savedSignIn = manifest.contract.capabilities.includes("provider-saved-signin");
  if (signIn && (manifest.contract.version.minor < 5 || !manifest.contract.capabilities.includes("account-auth"))) fail();
  if (savedSignIn && !signIn) fail();
  if (manifest.authBindings === undefined) return;
  if (!Array.isArray(manifest.authBindings) || manifest.authBindings.length > 64) fail();
  if (!manifest.authBindings.length) return;
  if (manifest.contract.version.minor < 5 || !manifest.contract.capabilities.includes("account-auth") || manifest.component != null) fail();
  const seen = new Set(), providers = new Set();
  for (const binding of manifest.authBindings) {
    const providerSignIn = binding?.from === "provider:signin";
    if (!binding || typeof binding !== "object" || !id(binding.id) || seen.has(binding.id)
      || typeof binding.from !== "string"
      || (providerSignIn ? !signIn || providers.has(binding.provider)
        : !manifest.window || !/^window:(?:page|panel:(?:0|[1-9][0-9]*))$/.test(binding.from))
      || binding.from.startsWith("window:panel:") && Number(binding.from.slice(13)) > 4294967295
      || !manifest.providers?.some(provider => provider.id === binding.provider)
      || !Array.isArray(binding.methods) || !binding.methods.length || binding.methods.length > 16
      || binding.reauthorize !== undefined && typeof binding.reauthorize !== "boolean") fail();
    const methods = new Set();
    for (const method of binding.methods) {
      if (!method || !id(method.id) || method.mode !== "poll" || methods.has(method.id)) fail();
      if (providerSignIn ? !word(method.label) : (method.label ?? "") !== "" || method.fields?.length) fail();
      validateFields(method.fields);
      if (method.id === "saved-signin" && (!savedSignIn || !providerSignIn || method.fields?.length !== 1
        || method.fields[0].key !== "record" || method.fields[0].type !== "secret" || method.fields[0].optional)) fail();
      methods.add(method.id);
    }
    if (Buffer.byteLength(binding.from) > 256) fail();
    seen.add(binding.id);
    if (providerSignIn) providers.add(binding.provider);
  }
}
