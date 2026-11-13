import { Buffer } from "node:buffer";
import { isIP } from "node:net";
import { posix } from "node:path";
import { MAX_CHUNK_BYTES, SDKError } from "./bytes.js";

export const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
export const validID = value => typeof value === "string" && value.length <= 128 && /^[a-z0-9][a-z0-9._-]*$/.test(value);
export const componentID = manifest => manifest.component?.id ?? "primary";
export const actionOwner = action => action.component || "primary";
export const MAX_MODULE_SERVICES = 16;
const fail = message => { throw new SDKError("invalid", message); };
const uint32 = value => Number.isInteger(value) && value >= 0 && value <= 4294967295;
export const validServiceContract = value => object(value) && validID(value.id) && uint32(value.version) && value.version !== 0;
const packagePath = value => typeof value === "string" && value !== "" && Buffer.byteLength(value) <= 1024
  && value !== "." && value !== ".." && !value.startsWith("/") && !value.startsWith("../")
  && posix.normalize(value).replace(/\/+$/, "") === value && !/[\\:\0?#%\p{Cc}]/u.test(value);
const list = (value, limit, name) => {
  if (value == null) return [];
  if (!Array.isArray(value) || value.length > limit) fail(`Invalid ${name}`);
  return value;
};
const uniqueIDs = (values, name) => {
  if (new Set(values).size !== values.length || values.some(value => !validID(value))) fail(`Invalid or duplicate ${name}`);
};
const schemaBytes = value => Buffer.byteLength(JSON.stringify(value,
  (_, item) => typeof item === "bigint" ? JSON.rawJSON(item.toString()) : item));

const platforms = {
  aix: "ppc64", android: "386 amd64 arm arm64", darwin: "amd64 arm64", dragonfly: "amd64",
  freebsd: "386 amd64 arm arm64 riscv64", illumos: "amd64", ios: "amd64 arm64", js: "wasm",
  linux: "386 amd64 arm arm64 loong64 mips mipsle mips64 mips64le ppc64 ppc64le riscv64 s390x",
  netbsd: "386 amd64 arm arm64", openbsd: "386 amd64 arm arm64 ppc64 riscv64",
  plan9: "386 amd64 arm", solaris: "amd64", wasip1: "wasm", windows: "386 amd64 arm64",
};
function validPlatform(value) {
  if (typeof value !== "string") return false;
  const [os, arch, extra] = value.split("/");
  return extra === undefined && Object.hasOwn(platforms, os) && platforms[os].split(" ").includes(arch);
}

export function validReadyURL(value) {
  if (typeof value !== "string" || Buffer.byteLength(value) > 4096 || /[\p{Cc}]/u.test(value)) return false;
  // Preserve explicit default ports, which URL normalizes away.
  const match = /^(https?):\/\/(\[[^\]]+\]|[^/:?#@]+):([0-9]+)(?:[/?][^#]*)?$/i.exec(value);
  if (!match || Number(match[3]) < 1 || Number(match[3]) > 65535) return false;
  const host = match[2].replace(/^\[|\]$/g, "");
  if (host === "localhost") return true;
  if (isIP(host) === 4) return host.split(".")[0] === "127";
  if (isIP(host) === 6) {
    const normalized = new URL(`http://[${host}]/`).hostname;
    return normalized === "[::1]" || /^\[::ffff:7f[0-9a-f]{2}:[0-9a-f]+\]$/i.test(normalized);
  }
  return false;
}

export function validateComposite(manifest) {
  const has = cap => manifest.contract.capabilities.includes(cap);
  const composite = has("module-rpc") || ["actions", "services", "compatibility", "window", "bindings", "component"].some(key => manifest[key] != null);
  if (!composite) return;
  if (manifest.contract.version.minor < 1) fail("Module composition requires SDK API 1.1");
  const actions = list(manifest.actions, 1024, "module actions");
  const services = list(manifest.services, MAX_MODULE_SERVICES, "module services");
  const bindings = list(manifest.bindings, 1024, "module bindings");
  if (actions.length + services.length + bindings.length > 1024) fail("Too many module composition descriptors");
  const own = componentID(manifest);
  if (manifest.component != null && (!object(manifest.component) || !validID(own) || own === "primary"
    || !validServiceContract(manifest.component.contract) || services.length || manifest.window != null || bindings.length
    || manifest.compatibility != null || manifest.contract.capabilities.length !== 1 || !has("module-rpc"))) fail("Invalid service component identity or delegated capability");
  const byService = new Map();
  for (const service of services) {
    if (!object(service) || !validID(service.id) || service.id === "primary" || byService.has(service.id)
      || !["exec", "bun"].includes(service.runtime) || !packagePath(service.entry)
      || !validServiceContract(service.contract) || !["lazy", "eager"].includes(service.start)) fail("Invalid or duplicate module service");
    const args = list(service.args, 128, "service arguments");
    if (args.some(arg => typeof arg !== "string" || Buffer.byteLength(arg) > MAX_CHUNK_BYTES || arg.includes("\0"))) fail("Invalid module service argument");
    const dependencies = list(service.dependsOn, 128, "service dependencies");
    const methods = list(service.methods, 1024, "service methods");
    uniqueIDs(methods, "service action method");
    const ready = service.ready ?? {};
    if (!object(ready) || ready.timeoutMs !== undefined && (!Number.isInteger(ready.timeoutMs) || ready.timeoutMs < 0 || ready.timeoutMs > 300000)) fail("Invalid service readiness");
    uniqueIDs(list(ready.httpEndpoints, 128, "service HTTP endpoints"), "service HTTP endpoint");
    byService.set(service.id, { service, dependencies, methods });
  }
  const colors = new Map();
  function visit(id) {
    if (colors.get(id) === 1) fail("Module service dependency cycle");
    if (colors.get(id) === 2) return;
    colors.set(id, 1);
    const { dependencies } = byService.get(id);
    if (new Set(dependencies).size !== dependencies.length) fail("Duplicate service dependency");
    for (const dependency of dependencies) {
      if (dependency === id || !byService.has(dependency)) fail("Unknown or self service dependency");
      visit(dependency);
    }
    colors.set(id, 2);
  }
  for (const id of byService.keys()) visit(id);
  const byAction = new Map();
  for (const action of actions) {
    if (!object(action) || !has("module-rpc") || !validID(action.id) || byAction.has(action.id)
      || !["read", "write"].includes(action.effect) || !(object(action.inputSchema) || typeof action.inputSchema === "boolean")
      || schemaBytes(action.inputSchema) > MAX_CHUNK_BYTES
      || action.component != null && typeof action.component !== "string") fail("Invalid or duplicate module action");
    const owner = actionOwner(action);
    if (manifest.component != null ? owner !== own : owner !== "primary" && !byService.get(owner)?.methods.includes(action.id)) fail("Action owner does not declare its service method");
    byAction.set(action.id, action);
  }
  for (const { service, methods } of byService.values()) {
    if (methods.some(method => actionOwner(byAction.get(method) ?? {}) !== service.id || !byAction.has(method))) fail("Service method does not name its own action");
  }
  if (manifest.window != null && (!object(manifest.window) || !packagePath(manifest.window.metadata)
    || posix.basename(manifest.window.metadata) !== "package.json")) fail("Window metadata must name a relative package.json");
  const seenBindings = new Set();
  for (const binding of bindings) {
    if (!object(binding) || !validID(binding.id) || seenBindings.has(binding.id)
      || typeof binding.from !== "string" || !(binding.from === "primary" || manifest.window != null
        && ["window:page", "window:panel"].some(kind => binding.from === kind || binding.from.startsWith(kind + ":") && validID(binding.from.slice(kind.length + 1))))) fail("Invalid or duplicate module binding");
    if (binding.action) {
      if (!byAction.has(binding.action) || binding.service || binding.endpoint) fail("Binding requires one declared action or endpoint");
    } else if (!byService.get(binding.service)?.service.ready?.httpEndpoints?.includes(binding.endpoint)) fail("Binding references an undeclared service endpoint");
    seenBindings.add(binding.id);
  }
  if (manifest.compatibility != null) {
    const value = manifest.compatibility;
    if (!object(value) || !["^1", "^1.1", "^1.2"].includes(value.hostAPI)
      || value.nativeABI !== undefined && !uint32(value.nativeABI)) fail("Unsupported host API requirement");
    const values = list(value.platforms, 128, "compatibility platforms");
    if (new Set(values).size !== values.length || values.some(value => !validPlatform(value))) fail("Invalid or duplicate compatibility platform");
  }
}

export function checkHostCompatibility(manifest, platform) {
  if (manifest.compatibility?.nativeABI && manifest.compatibility.nativeABI !== 1) fail("Module requires an unsupported native ABI");
  if (manifest.compatibility?.platforms?.length && !manifest.compatibility.platforms.includes(platform)) fail(`Module does not support host platform ${platform}`);
}
