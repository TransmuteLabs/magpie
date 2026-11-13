import { Buffer } from "node:buffer";
import { posix } from "node:path";
import { MAX_CHUNK_BYTES, MAX_FRAME_BYTES, SDKError } from "./bytes.js";
import { actionOwner, componentID, validateComposite, checkHostCompatibility } from "./composite.js";

const capabilities = {
  "inbound-codec": ["inbound.decode", "inbound.response"],
  "outbound-executor": ["provider.execute"],
  "agent-wiring": ["agent.plan"],
  "account-auth": ["auth.start", "auth.finish", "auth.refresh", "auth.logout"],
  "model-discovery": ["provider.models"],
  "usage-reporter": ["provider.usage"],
  "operation-control": ["operation.status", "operation.cancel"],
  "tool-provider": ["tool.call"],
  "session-migration": ["session.convert"],
  "module-rpc": ["module.call"],
};

export const CONTRACT = Object.freeze({
  version: Object.freeze({ major: 1, minor: 1 }),
  capabilities: Object.freeze(Object.keys(capabilities)),
});

export function parseJSON(text) {
  return JSON.parse(text, (key, value, context) => {
    if (typeof value !== "number") return value;
    if (!Number.isFinite(value)) throw new SDKError("invalid", "Invalid JSON number");
    if (Number.isInteger(value) && !Number.isSafeInteger(value) && /^-?[0-9]+$/.test(context?.source || "")) {
      return BigInt(context.source);
    }
    return value;
  });
}

export function stringifyJSON(value) {
  return JSON.stringify(value, (key, item) => {
    if (typeof item === "number" && !Number.isFinite(item)) throw new SDKError("invalid", "JSON numbers must be finite");
    return typeof item === "bigint" ? JSON.rawJSON(item.toString()) : item;
  });
}

export function jsonSnapshot(value) {
  const text = stringifyJSON(value);
  if (text === undefined || Buffer.byteLength(text) > MAX_FRAME_BYTES) throw new SDKError("invalid", "Invalid or oversized JSON value");
  return parseJSON(text);
}

export function freezeJSON(value) {
  const pending = [value];
  while (pending.length) {
    const item = pending.pop();
    if (item && typeof item === "object" && !Object.isFrozen(item)) {
      for (const child of Object.values(item)) pending.push(child);
      Object.freeze(item);
    }
  }
  return value;
}

const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const control = /[\p{Cc}]/u;
const space = /[\s\p{Cc}]/u;
const validID = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
const validName = value => typeof value === "string" && !!value.trim() && Buffer.byteLength(value) <= 256 && !control.test(value);
const fail = message => { throw new SDKError("invalid", message); };

function validVersion(value) {
  if (typeof value !== "string" || value.length > 128) return false;
  const match = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$/.exec(value);
  if (!match || match.slice(1, 4).some(part => BigInt(part) > 18446744073709551615n)) return false;
  return [match[4], match[5]].every((part, index) => part === undefined || part.split(".").every(id =>
    !!id && !(index === 0 && /^0[0-9]+$/.test(id))));
}

function endpoint(value) {
  if (typeof value !== "string" || value === "/" || !value.startsWith("/") || posix.normalize(value).replace(/\/+$/, "") !== value
    || /[{}?%#\\]/.test(value) || space.test(value)) return false;
  const reserved = ["/api", "/_magpie", "/agent", "/v1", "/v1beta", "/models", "/chat/completions",
    "/responses", "/messages", "/images", "/embeddings", "/rerank", "/decisions", "/audio", "/videos",
    "/mcp", "/backend-api", "/muse-code", "/alpha/search", "/magpie-ca.crt"];
  return !reserved.some(prefix => value === prefix || value.startsWith(prefix + "/")) && !value.startsWith("/wsl-");
}

function homePath(value) {
  return typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 1024
    && value !== "." && value !== ".." && !value.startsWith("/") && !value.startsWith("../")
    && posix.normalize(value).replace(/\/+$/, "") === value && !/[\\:\0*?[\]]/.test(value)
    && !control.test(value) && !value.toLowerCase().includes(".magpie-extension");
}

function modelPolicy(model) {
  if (!object(model)) fail("Invalid model policy");
  const fields = ["input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"];
  if (model.price !== undefined && model.price !== null) {
    const price = model.price;
    if (!object(price) || Object.keys(price).some(field => !["currency", ...fields].includes(field))
      || price.currency !== undefined && price.currency !== "" && price.currency !== "USD") fail("Invalid model price");
    for (const field of fields) {
      if (!Object.hasOwn(price, field)) continue;
      const value = typeof price[field] === "bigint" ? Number(price[field]) : price[field];
      if (typeof value !== "number" || !Number.isFinite(value) || value < 0) fail("Model prices must be finite and nonnegative");
      price[field] = value;
    }
  }
  if (model.efforts !== undefined) {
    const levels = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];
    if (!Array.isArray(model.efforts) || model.efforts.length > levels.length
      || new Set(model.efforts).size !== model.efforts.length
      || model.efforts.some(level => !levels.includes(level))) fail("Invalid or duplicate model effort");
  }
}

export function requiredMethods(manifest) {
  const methods = [];
  for (const capability of manifest.contract.capabilities) {
    if (capability === "model-discovery" && !manifest.providers?.length) continue;
    methods.push(...capabilities[capability]);
    if (capability === "agent-wiring" && manifest.agents?.some(agent => agent.bin || agent.configFiles?.length || agent.fields?.length)) {
      methods.push("agent.inspect");
    }
  }
  return methods.sort();
}

export function validateManifest(value) {
  let manifest;
  try { manifest = jsonSnapshot(value); } catch { fail("Invalid extension manifest"); }
  if (!object(manifest) || manifest.schema !== 1 || !validID(manifest.id) || !validVersion(manifest.version)) fail("Invalid extension identity or schema");
  if (!["exec", "bun", "native"].includes(manifest.runtime)) fail("Unsupported extension runtime");
  if (typeof manifest.entry !== "string" || !manifest.entry || manifest.entry.startsWith("/")
    || manifest.entry === "." || manifest.entry === ".." || manifest.entry.startsWith("../")
    || posix.normalize(manifest.entry).replace(/\/+$/, "") !== manifest.entry || /[\\:]/.test(manifest.entry)
    || control.test(manifest.entry) || Buffer.byteLength(manifest.entry) > 1024) fail("Invalid extension entry");
  if (manifest.args !== undefined && (!Array.isArray(manifest.args) || manifest.args.length > 128
    || manifest.args.some(arg => typeof arg !== "string" || Buffer.byteLength(arg) > MAX_CHUNK_BYTES || arg.includes("\0")))) fail("Invalid extension arguments");
  if (manifest.runtime === "native" && manifest.args?.length) fail("Native extensions do not take process arguments");
  const contract = manifest.contract;
  if (!object(contract) || !object(contract.version) || contract.version.major !== 1 || ![0, 1].includes(contract.version.minor)
    || !Array.isArray(contract.capabilities) || !contract.capabilities.length
    || new Set(contract.capabilities).size !== contract.capabilities.length
    || contract.capabilities.some(capability => !Object.hasOwn(capabilities, capability))) fail("Unsupported extension contract or capabilities");
  const has = capability => contract.capabilities.includes(capability);
  const groups = ["inbounds", "providers", "agents", "sessions", "tools"];
  if (groups.some(key => manifest[key] !== undefined && !Array.isArray(manifest[key]))
    || groups.reduce((count, key) => count + (manifest[key]?.length || 0), 0) > 1024) fail("Invalid extension descriptor lists");
  for (const key of groups) {
    const ids = new Set(), paths = new Set();
    for (const descriptor of manifest[key] || []) {
      const id = key === "tools" ? descriptor?.name : descriptor?.id;
      if (!object(descriptor) || !validID(id) || ids.has(id)) fail("Invalid or duplicate extension descriptor");
      ids.add(id);
      if (key === "inbounds") {
        if (!has("inbound-codec") || !endpoint(descriptor.path) || paths.has(descriptor.path)) fail("Invalid, reserved or duplicate inbound path");
        paths.add(descriptor.path);
      } else if (key === "providers") {
        const protocols = descriptor.protocols;
        if (!(has("outbound-executor") || has("model-discovery")) || !validName(descriptor.name)
          || !Array.isArray(protocols) || !protocols.length || new Set(protocols).size !== protocols.length
          || protocols.some(protocol => !["chat", "responses", "messages", "anthropic", "gemini"].includes(protocol))
          || descriptor.models !== undefined && !Array.isArray(descriptor.models)) fail("Invalid provider descriptor");
        const models = new Set();
        for (const model of descriptor.models || []) {
          if (!object(model) || typeof model.id !== "string" || !model.id || Buffer.byteLength(model.id) > 300
            || space.test(model.id) || models.has(model.id)
            || ["contextWindow", "maxOutput"].some(field => model[field] !== undefined && (!Number.isSafeInteger(model[field]) || model[field] < 0))
            || model.protocols !== undefined && (!Array.isArray(model.protocols) || model.protocols.some(protocol => !protocols.includes(protocol)))) fail("Invalid or duplicate provider model");
          modelPolicy(model);
          models.add(model.id);
        }
      } else if (key === "tools") {
        if (!has("tool-provider") || !(object(descriptor.inputSchema) || typeof descriptor.inputSchema === "boolean")
          || Buffer.byteLength(JSON.stringify(descriptor.inputSchema)) > MAX_CHUNK_BYTES) fail("Invalid tool descriptor");
      } else {
        if (!has(key === "agents" ? "agent-wiring" : "session-migration") || !validName(descriptor.name)) fail("Invalid agent or session descriptor");
        if (key === "agents") {
          const files = descriptor.configFiles ?? [], fields = descriptor.fields ?? [];
          if (descriptor.bin !== undefined && (typeof descriptor.bin !== "string" || descriptor.bin !== "" && !validID(descriptor.bin))
            || !Array.isArray(files) || !Array.isArray(fields) || files.length > 128 || fields.length > 128) fail("Invalid agent wiring metadata");
          const seenFiles = new Set(), seenFields = new Set();
          for (const file of files) {
            if (!homePath(file) || seenFiles.has(file.toLowerCase())) fail("Invalid or duplicate agent config path");
            seenFiles.add(file.toLowerCase());
          }
          for (const field of fields) {
            if (!object(field) || !validID(field.key) || !validName(field.label) || seenFields.has(field.key)) fail("Invalid or duplicate agent field");
            seenFields.add(field.key);
          }
        } else {
          if (["from", "to", "outputDir"].some(field => descriptor[field] !== undefined && typeof descriptor[field] !== "string")) fail("Invalid session routing metadata");
          if ((descriptor.from || descriptor.to || descriptor.outputDir)
            && (!validID(descriptor.from) || !validID(descriptor.to) || descriptor.from === descriptor.to || !homePath(descriptor.outputDir))) fail("Invalid session format route or output directory");
        }
      }
    }
  }
  if (has("inbound-codec") && !manifest.inbounds?.length || has("tool-provider") && !manifest.tools?.length
    || has("model-discovery") && !manifest.providers?.length && !has("inbound-codec")
    || !manifest.providers?.length && ["outbound-executor", "account-auth", "usage-reporter"].some(has)
    || has("agent-wiring") && !manifest.agents?.length
    || has("session-migration") && !manifest.sessions?.length) fail("Missing capability descriptors");
  validateComposite(manifest);
  return freezeJSON(manifest);
}

export function publicManifest(manifest) {
  const result = jsonSnapshot(manifest);
  delete result.runtime;
  delete result.entry;
  delete result.args;
  for (const key of ["services", "window", "bindings", "compatibility"]) delete result[key];
  if (result.actions != null) result.actions = result.actions.filter(action => actionOwner(action) === componentID(manifest));
  for (const provider of result.providers || []) for (const model of provider.models || []) modelPolicy(model);
  return freezeJSON(result);
}

export function validateModule(module) {
  if (!object(module) || module.handlers !== undefined && !object(module.handlers)
    || module.actions !== undefined && !object(module.actions) || module.start !== undefined && typeof module.start !== "function") fail("Invalid extension module");
  const manifest = validateManifest(module.manifest), required = requiredMethods(manifest);
  if (module.start && manifest.contract.version.minor < 1) fail("Module startup requires SDK API 1.1");
  const handlers = new Map(Object.entries(module.handlers ?? {}));
  if (handlers.has("module.call")) fail("module.call is owned by the SDK; register module.actions");
  const actions = new Map(Object.entries(module.actions ?? {}));
  const declared = new Map((manifest.actions ?? []).filter(action => actionOwner(action) === componentID(manifest)).map(action => [action.id, action]));
  if ([...declared.keys()].some(id => typeof actions.get(id) !== "function")
    || [...actions].some(([id, handler]) => !declared.has(id) || typeof handler !== "function")) fail("Missing or undeclared module action handler");
  if (manifest.contract.capabilities.includes("module-rpc")) handlers.set("module.call", (ctx, call, stream) => {
    const params = call.params;
    if (!object(params) || Object.keys(params).some(key => !["action", "input"].includes(key)) || !Object.hasOwn(params, "input")
      || params.action != null && typeof params.action !== "string") {
      throw new SDKError("invalid_params", "module.call requires a declared action and JSON input");
    }
    if (!declared.has(params.action)) throw new SDKError("method_not_found", "Module action is not owned by this component");
    return actions.get(params.action)(ctx, freezeJSON({ ...call, action: params.action, params: params.input }), stream);
  });
  if (required.some(method => typeof handlers.get(method) !== "function")
    || [...handlers].some(([method, handler]) => !required.includes(method) || typeof handler !== "function")) fail("Missing or undeclared extension handler");
  return { manifest, handlers, start: module.start };
}

export function serviceManifest(root, id) {
  root = validateManifest(root);
  if (root.component != null) fail("A service manifest requires the root module");
  const service = root.services?.find(service => service.id === id);
  if (!service) fail("Unknown module service");
  return validateManifest({
    schema: root.schema, id: root.id, version: root.version,
    runtime: service.runtime, entry: service.entry, ...(service.args ? { args: service.args } : {}),
    contract: { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
    component: { id, contract: service.contract },
    actions: (root.actions ?? []).filter(action => actionOwner(action) === id),
  });
}

export function checkCompatibility(value, platform = `${process.platform === "win32" ? "windows" : process.platform}/${({ x64: "amd64", ia32: "386" })[process.arch] ?? process.arch}`) {
  const manifest = validateManifest(value);
  checkHostCompatibility(manifest, platform);
  return manifest;
}
