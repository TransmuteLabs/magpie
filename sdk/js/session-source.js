import { Buffer } from "node:buffer";
import { posix } from "node:path";
import { SDKError, MAX_FRAME_BYTES, decodeBytes } from "./bytes.js";
import { validateSessionResume } from "./session-resume.js";

const fail = message => { throw new SDKError("invalid_params", message); };
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && /^[a-z0-9][a-z0-9._-]{0,127}$/.test(value);
const clean = value => typeof value === "string" && Buffer.byteLength(value) <= 1024 &&
  value !== "." && value !== ".." && !value.startsWith("/") && !value.startsWith("../") &&
  posix.normalize(value) === value && !/[\\:\x00-\x1f]/.test(value) && !/\p{Cc}/u.test(value) && !value.toLowerCase().includes(".magpie-extension");
export { clean as sessionSourcePath };
const keys = (value, allowed) => {
  if (!object(value) || Object.keys(value).some(key => !allowed.includes(key))) fail("Unknown session source field");
};
const counter = value => {
  if (typeof value !== "bigint" && (!Number.isSafeInteger(value) || value < 0)) fail("Invalid source counter");
  const number = BigInt(value);
  if (number < 0n || number > 9223372036854775807n) fail("Source counter overflow");
  return number;
};
const stamp = value => {
  if (typeof value !== "string" ||
    !/^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$/.test(value)) return false;
  const year = Number(value.slice(0, 4)), month = Number(value.slice(5, 7)), day = Number(value.slice(8, 10));
  const leap = year % 400 === 0 || year % 4 === 0 && year % 100 !== 0;
  return day <= [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31][month - 1];
};

const descendantRule = rule => object(rule) && Object.keys(rule).every(key => ["kind", "excludeDirectories"].includes(key)) &&
  rule.kind === "journal-stem-jsonl" && (rule.excludeDirectories === undefined ||
    Array.isArray(rule.excludeDirectories) && rule.excludeDirectories.length <= 16 &&
    rule.excludeDirectories.every(name => id(name) && !name.includes("/")) &&
    new Set(rule.excludeDirectories).size === rule.excludeDirectories.length);

export function sessionSourceDescendantPath(rule, journal, name) {
  if (!descendantRule(rule) || !clean(journal) || !clean(name) || !journal.endsWith(".jsonl")) return false;
  const prefix = journal.slice(0, -6) + "/";
  if (!name.startsWith(prefix) || !name.endsWith(".jsonl")) return false;
  return !name.slice(prefix.length).split("/").slice(0, -1).some(part => rule.excludeDirectories?.includes(part));
}

export function sessionSourceID(layout, name) {
  if (!clean(name)) fail("Invalid source locator");
  const parts = name.split("/");
  let value = "";
  if (layout.kind === "flat" && layout.idFrom === "filename-stem" && !layout.entry && parts.length === 1 && name.endsWith(".jsonl")) {
    value = name.slice(0, -6);
  } else if (layout.kind === "bucketed" && layout.idFrom === "filename-after-first-underscore" && !layout.entry &&
    parts.length === 2 && parts[1].endsWith(".jsonl") && parts[1].includes("_")) {
    value = parts[1].slice(parts[1].indexOf("_") + 1, -6);
  } else if (layout.kind === "directory" && layout.idFrom === "parent-directory" && layout.entry === "log.jsonl" &&
    parts.length === 2 && parts[1] === "log.jsonl") value = parts[0];
  if (!/^[A-Za-z0-9_-]{1,128}$/.test(value)) fail("Session locator does not match its finite layout");
  return value;
}

export function validateSessionSources(manifest) {
  const sources = manifest.sessionSources ?? [];
  const has = manifest.contract.capabilities.includes("session-source");
  if (!Array.isArray(sources) || sources.length > 128 || has !== (sources.length > 0) ||
    sources.length && manifest.contract.version.minor < 6) fail("Session sources require API 1.6 and session-source");
  const seen = new Set();
  for (const source of sources) {
    keys(source, ["id", "agent", "codec", "container", "stores", "layout", "transcript", "resume", "descendants", "agentResume"]);
    if (!id(source.id) || !id(source.agent) || !id(source.codec) || seen.has(source.id) ||
      source.container !== "jsonl" || typeof source.transcript !== "boolean" ||
      !Array.isArray(source.stores) || source.stores.length < 1 || source.stores.length > 16) fail("Invalid session source descriptor");
    seen.add(source.id);
    validateSessionResume(manifest, source.agent, source.resume, source.agentResume);
    keys(source.layout, ["kind", "idFrom", "entry"]);
    sessionSourceID(source.layout, ({ flat: "id.jsonl", bucketed: "bucket/time_id.jsonl", directory: "id/log.jsonl" })[source.layout.kind]);
    if (source.descendants !== undefined && (manifest.contract.version.minor < 9 || source.layout.kind === "directory" ||
      !descendantRule(source.descendants))) fail("Source descendants require API 1.9 and a bounded journal tree");
    const slots = new Set();
    for (const store of source.stores) {
      keys(store, ["slot", "nativeRootSlot", "homeRelativePath"]);
      if (["nativeRootSlot", "homeRelativePath"].some(key => store[key] !== undefined && typeof store[key] !== "string")) fail("Invalid source store selector");
      if (!id(store.slot) || slots.has(store.slot) || Boolean(store.nativeRootSlot) === Boolean(store.homeRelativePath)) fail("Invalid source store");
      slots.add(store.slot);
      if (store.homeRelativePath && (!clean(store.homeRelativePath) || /[*?[\]~$]/.test(store.homeRelativePath))) fail("Invalid source HOME root");
      if (store.nativeRootSlot && !["claude.projects", "codex.sessions", "codex.archived-sessions", "omp.sessions", "devin.file-sessions"].includes(store.nativeRootSlot)) fail("Unknown native source root");
    }
  }
}

export function validateSessionSourceRequest(manifest, request, stringify) {
  keys(request, ["sourceId", "locator", "snapshot", "mode", "documents", "limits", "descendants"]);
  const source = manifest.sessionSources?.find(item => item.id === request.sourceId);
  if (!source || !["summary", "transcript"].includes(request.mode) || request.mode === "transcript" && !source.transcript) fail("Unsupported source read");
  keys(request.locator, ["storeRef", "path", "sessionId"]);
  keys(request.limits, ["maxParts", "maxTextBytes"]);
  if (sessionSourceID(source.layout, request.locator.path) !== request.locator.sessionId ||
    typeof request.locator.storeRef !== "string" || !request.locator.storeRef || Buffer.byteLength(request.locator.storeRef) > 128 ||
    !/^[a-f0-9]{64}$/.test(request.snapshot) ||
    !Number.isInteger(request.limits.maxParts) || request.limits.maxParts < 1 || request.limits.maxParts > 10000 ||
    !Number.isInteger(request.limits.maxTextBytes) || request.limits.maxTextBytes < 1 || request.limits.maxTextBytes > 400000 ||
    !Array.isArray(request.documents) || request.documents.length < 1 || request.documents.length > 128) fail("Invalid source identity or limits");
  const seen = new Set();
  let total = 0;
  for (const [index, document] of request.documents.entries()) {
    keys(document, ["path", "format", "body"]);
    if (!clean(document.path) || seen.has(document.path) || document.format !== source.codec ||
      index === 0 && document.path !== request.locator.path) fail("Invalid source document");
    seen.add(document.path);
    total += decodeBytes(document.body, 16 << 20).length;
    if (total > 16 << 20) fail("Source binary budget exceeded");
  }
  if (request.descendants !== undefined && (manifest.contract.version.minor < 9 || !source.descendants ||
    !Array.isArray(request.descendants) || request.descendants.length >= 128 ||
    new Set(request.descendants).size !== request.descendants.length ||
    request.descendants.some(name => !seen.has(name) || !sessionSourceDescendantPath(source.descendants, request.locator.path, name)))) {
    fail("Source descendant is outside its declared admitted documents");
  }
  if (Buffer.byteLength(stringify(request)) > MAX_FRAME_BYTES - 4096) fail("Source encoded frame budget exceeded");
  return request;
}

export function validateSessionSourceResult(request, result, stringify) {
  keys(result, ["mode", "sessionId", "summary", "parts", "cut"]);
  if (result.cut !== undefined && typeof result.cut !== "boolean") fail("Invalid source cut flag");
  if (result.mode !== request.mode || result.sessionId !== request.locator.sessionId ||
    Buffer.byteLength(stringify(result)) > MAX_FRAME_BYTES - 4096) fail("Source result changed identity or exceeded frame");
  if (request.mode === "summary") {
    if (!object(result.summary) || result.parts !== undefined || result.cut !== undefined) fail("Invalid source summary");
    const summary = result.summary;
    keys(summary, ["title", "cwd", "start", "last", "parentId", "rootId", "usage", "subs"]);
    if (typeof summary.title !== "string" || Buffer.byteLength(summary.title) > 4096 ||
      typeof summary.cwd !== "string" || Buffer.byteLength(summary.cwd) > 4096 || summary.cwd.includes("\0") ||
      ["start", "last"].some(key => summary[key] !== undefined && (typeof summary[key] !== "string" || summary[key] !== "" && !stamp(summary[key]))) ||
      ["parentId", "rootId"].some(key => summary[key] !== undefined && (typeof summary[key] !== "string" || Buffer.byteLength(summary[key]) > 128)) ||
      summary.usage !== undefined && (!Array.isArray(summary.usage) || summary.usage.length > 10000)) fail("Invalid source summary metadata");
    const identities = summary.subs ?? [];
    if (summary.subs !== undefined && !Array.isArray(summary.subs) || !Array.isArray(identities) ||
      identities.length !== (request.descendants?.length ?? 0) || identities.length >= 128) fail("Source identities must cover admitted descendants");
    const admitted = new Set(request.descendants ?? []), identitiesSeen = new Set([request.locator.sessionId]);
    if (admitted.size !== (request.descendants?.length ?? 0)) fail("Duplicate admitted source descendant");
    for (const identity of identities) {
      keys(identity, ["path", "sessionId"]);
      if (!admitted.delete(identity.path) || !clean(identity.sessionId) || identity.sessionId.includes("/") ||
        Buffer.byteLength(identity.sessionId) > 128 || identitiesSeen.has(identity.sessionId)) fail("Invalid source descendant identity");
      identitiesSeen.add(identity.sessionId);
    }
    let total = 0n;
    for (const point of summary.usage ?? []) {
      keys(point, ["at", "model", "input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"]);
      if (!stamp(point.at) || typeof point.model !== "string" || !point.model || Buffer.byteLength(point.model) > 300) fail("Invalid source usage");
      if (counter(point.cacheWrite1h) > counter(point.cacheWrite)) fail("One-hour cache writes exceed total cache writes");
      for (const key of ["input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"]) {
        total += counter(point[key]);
        if (total > 9223372036854775807n) fail("Source counter overflow");
      }
    }
  } else {
    if (result.summary !== undefined || !Array.isArray(result.parts) || result.parts.length > request.limits.maxParts ||
      typeof result.cut !== "boolean") fail("Invalid source transcript");
    let total = 0;
    for (const part of result.parts) {
      keys(part, ["role", "kind", "text", "name", "model", "cut"]);
      if (!["user", "assistant", "tool"].includes(part.role) ||
        !["text", "thinking", "tool_use", "tool_result", "context", "image"].includes(part.kind) ||
        typeof part.text !== "string" || part.cut !== undefined && (!Number.isSafeInteger(part.cut) || part.cut < 0 || part.cut > (16 << 20)) ||
        part.name !== undefined && (typeof part.name !== "string" || Buffer.byteLength(part.name) > 1024) ||
        part.model !== undefined && (typeof part.model !== "string" || Buffer.byteLength(part.model) > 300)) fail("Invalid source transcript part");
      total += Buffer.byteLength(part.text);
      if (total > request.limits.maxTextBytes) fail("Source transcript budget exceeded");
    }
  }
  return result;
}
