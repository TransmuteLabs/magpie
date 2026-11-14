import { SDKError } from "./bytes.js";

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const bytes = value => typeof value === "string" ? Buffer.byteLength(value) : Infinity;
const fail = message => { throw new SDKError("invalid_params", message); };
const optionFields = new Set(["key", "label", "description", "default", "confirm",
  "labels", "descriptions", "confirms", "filtersModels", "placement"]);
const text = (value, limit) => value === undefined || typeof value === "string"
  && bytes(value) <= limit && !value.includes("\0");
const words = (value, limit) => value === undefined || object(value)
  && Object.keys(value).length <= 16 && Object.entries(value).every(([language, value]) =>
    language.length > 0 && bytes(language) <= 35 && !/[/\\\s\0]/.test(language) && text(value, limit));

export function validateProviderOptions(manifest) {
  const declared = new Map();
  for (const provider of manifest.providers || []) {
    if (provider.options === undefined) continue;
    if (!Array.isArray(provider.options) || provider.options.length > 32
      || provider.options.length && (manifest.contract.version.minor < 9 || manifest.component != null))
      fail("Provider switches require SDK API 1.9 and a root provider");
    const seen = new Set();
    for (const option of provider.options) {
      if (!object(option) || typeof option.key !== "string"
        || Object.keys(option).some(field => !optionFields.has(field))
        || !/^[a-z][a-zA-Z0-9_-]{0,39}$/.test(option.key)
        || ["constructor", "prototype"].includes(option.key) || seen.has(option.key)
        || typeof option.label !== "string" || !option.label.trim()
        || !text(option.label, 512) || !text(option.description, 4096) || !text(option.confirm, 4096)
        || !words(option.labels, 512) || !words(option.descriptions, 4096) || !words(option.confirms, 4096)
        || option.default !== undefined && typeof option.default !== "boolean"
        || option.filtersModels !== undefined && typeof option.filtersModels !== "boolean"
        || option.placement !== undefined && !["", "top"].includes(option.placement))
        fail("Invalid or duplicate provider switch");
      seen.add(option.key);
      const identity = JSON.stringify([
        option.label, option.description || "", option.default || false, option.confirm || "",
        ...["labels", "descriptions", "confirms"].map(key => Object.entries(option[key] || {}).sort()),
        option.filtersModels || false, option.placement || "",
      ]);
      if (declared.has(option.key) && declared.get(option.key) !== identity)
        fail("Provider switch has conflicting root configuration declarations");
      declared.set(option.key, identity);
    }
  }
}

export function providerOptionOn(option, values) {
  if (!object(values)) fail("Provider switch configuration must be an object");
  if (!Object.hasOwn(values, option.key)) return option.default === true;
  if (typeof values[option.key] !== "boolean") fail("Provider switch must be boolean");
  return values[option.key];
}

export function validateProviderOptionValues(manifest, values) {
  for (const provider of manifest.providers || [])
    for (const option of provider.options || []) providerOptionOn(option, values);
}
