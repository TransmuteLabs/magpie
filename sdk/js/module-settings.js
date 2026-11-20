import { Buffer } from "node:buffer";

const fields = ["name", "kind", "title", "description", "default", "enum", "minimum", "maximum"];
const text = (value, max) => typeof value === "string" && value.isWellFormed() &&
  Buffer.byteLength(value) <= max && !value.includes("\0");
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const number = value => typeof value === "bigint" ? Number(value) : value;

function validValue(setting, value) {
  switch (setting.kind) {
    case "boolean": return typeof value === "boolean";
    case "string": return text(value, 4096);
    case "integer":
      if (!Number.isSafeInteger(value)) return false;
      break;
    case "number":
      value = number(value);
      if (!Number.isFinite(value)) return false;
      break;
    default: return false;
  }
  return (setting.minimum === undefined || value >= setting.minimum) &&
    (setting.maximum === undefined || value <= setting.maximum);
}

export function validateModuleSettings(manifest, reject) {
  const settings = manifest.settings;
  if (settings == null || Array.isArray(settings) && settings.length === 0) return;
  if (!Array.isArray(settings) || settings.length > 32 || manifest.component != null || manifest.contract.version.minor < 16) {
    reject("Module settings require root SDK API 1.16 within descriptor limits");
  }
  const used = new Set((manifest.providers ?? []).flatMap(provider => (provider.options ?? []).map(option => option.key)));
  for (const setting of settings) {
    if (!object(setting) || Object.keys(setting).some(key => !fields.includes(key)) ||
      typeof setting.name !== "string" || !/^[a-z][a-zA-Z0-9_-]{0,39}$/.test(setting.name) ||
      ["constructor", "prototype"].includes(setting.name) || used.has(setting.name) ||
      !text(setting.title, 512) || !setting.title.trim() ||
      setting.description !== undefined && !text(setting.description, 4096)) {
      reject("Invalid or duplicate module setting");
    }
    const minimum = number(setting.minimum), maximum = number(setting.maximum);
    if ((minimum !== undefined || maximum !== undefined) && !["number", "integer"].includes(setting.kind) ||
      minimum !== undefined && !Number.isFinite(minimum) ||
      maximum !== undefined && !Number.isFinite(maximum) ||
      minimum !== undefined && maximum !== undefined && minimum > maximum) {
      reject("Invalid module setting bounds");
    }
    const options = setting.enum === undefined ? [] : setting.enum;
    if (!validValue(setting, setting.default) || !Array.isArray(options) || options.length > 32 ||
      options.some(value => !validValue(setting, value))) {
      reject("Invalid module setting default or enum");
    }
    const choices = setting.kind === "number" ? options.map(number) : options;
    const fallback = setting.kind === "number" ? number(setting.default) : setting.default;
    if (new Set(choices).size !== choices.length || choices.length && !choices.includes(fallback)) {
      reject("Invalid module setting default or enum");
    }
    used.add(setting.name);
  }
}
