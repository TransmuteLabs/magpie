import { Buffer } from "node:buffer";
import { posix } from "node:path";
import { SDKError, decodeBytes } from "./bytes.js";
import { validateSessionSourceResult } from "./session-source.js";
import { validateSessionResume } from "./session-resume.js";

const fail = message => { throw new SDKError("invalid_params", message); };
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const id = value => typeof value === "string" && /^[a-z0-9][a-z0-9._-]{0,127}$/.test(value);
const ref = value => typeof value === "string" && value.length > 0 && Buffer.byteLength(value) <= 1024 && !/[\0\r\n]/.test(value);
const path = value => ref(value) && value !== "." && value !== ".." && !value.startsWith("/") &&
  !value.endsWith("/") && posix.normalize(value) === value && !value.startsWith("../") && !/[\\:\p{Cc}]/u.test(value) &&
  !value.toLowerCase().includes(".magpie-extension");
const sessionID = value => typeof value === "string" && Buffer.byteLength(value) <= 128 && path(value) && !value.includes("/");
const keys = (value, allowed) => {
  if (!object(value) || Object.keys(value).some(key => !allowed.includes(key))) fail("Unknown storage source field");
};
const unique = values => new Set(values).size === values.length;
const optionalString = value => value == null || typeof value === "string";
export const STORAGE_METHODS = Object.freeze(["session.storage.list", "session.storage.read"]);
export const STORAGE_HOST_METHODS = Object.freeze(["storage.native", "storage.list", "storage.read", "storage.check"]);

export function storageFamilyAllows(family, name) {
  return !!(path(name) && (family.primary === name || family.sidecars?.includes(name) ||
    family.trees?.some(tree => name === tree || name.startsWith(tree + "/"))));
}

export function validateStorageSources(manifest) {
  const sources = manifest.storageSources ?? [];
  const enabled = manifest.contract.capabilities.includes("session-storage-source");
  if (!enabled && Array.isArray(sources) && sources.length === 0) return;
  if (!enabled || manifest.contract.version.minor < 8 || manifest.runtime !== "native" ||
    !Array.isArray(sources) || sources.length === 0 || sources.length > 16) fail("Storage sources require native SDK API 1.8");
  const ids = new Set((manifest.sessionSources ?? []).map(source => source.id));
  for (const source of sources) {
    keys(source, ["id", "agent", "codec", "roots", "families", "transcript", "resume", "agentResume"]);
    if (!id(source.id) || ids.has(source.id) || !id(source.agent) || !id(source.codec) ||
      typeof source.transcript !== "boolean" ||
      !Array.isArray(source.roots) || !source.roots.length || source.roots.length > 16 ||
      !Array.isArray(source.families) || !source.families.length || source.families.length > 16) fail("Invalid storage source descriptor");
    ids.add(source.id);
    validateSessionResume(manifest, source.agent, source.resume, source.agentResume);
    const slots = new Set(), families = new Set();
    for (const root of source.roots) {
      keys(root, ["slot", "homeRelativePath", "nativeRootSlot"]);
      if (!id(root.slot) || slots.has(root.slot) || !optionalString(root.homeRelativePath) || !optionalString(root.nativeRootSlot) ||
        Boolean(root.homeRelativePath) === Boolean(root.nativeRootSlot) ||
        root.homeRelativePath && (!path(root.homeRelativePath) || /[*?[\]]/.test(root.homeRelativePath)) ||
        root.nativeRootSlot && !id(root.nativeRootSlot)) fail("Invalid storage root");
      slots.add(root.slot);
    }
    for (const family of source.families) {
      keys(family, ["id", "primary", "sidecars", "trees"]);
      if (!id(family.id) || families.has(family.id) || !optionalString(family.primary) ||
        family.sidecars !== undefined && (!Array.isArray(family.sidecars) || family.sidecars.length > 8) ||
        family.trees !== undefined && (!Array.isArray(family.trees) || family.trees.length > 16) ||
        !family.primary && !family.trees?.length) fail("Invalid storage family");
      families.add(family.id);
      const paths = [...(family.primary ? [family.primary] : []), ...(family.sidecars ?? []), ...(family.trees ?? [])];
      if (!unique(paths) || paths.some(name => !path(name))) fail("Invalid storage family path");
    }
  }
}

export function validateStorageRequest(manifest, method, scope, auth, request) {
  keys(request, ["lease", "cursor", "limit", "locator", "mode"]);
  if (!optionalString(request.cursor) || !optionalString(request.mode) ||
    request.limit != null && !Number.isInteger(request.limit)) fail("Invalid storage request types");
  keys(request.lease, ["sourceId", "rootRef", "snapshotRef", "familyId", "limits"]);
  const lease = request.lease;
  const source = manifest.storageSources?.find(source => source.id === lease.sourceId);
  if (!STORAGE_METHODS.includes(method) || !manifest.contract.capabilities.includes("session-storage-source") ||
    manifest.contract.version.minor < 8 || !source || !source.families.some(family => family.id === lease.familyId) ||
    !ref(lease.rootRef) || !ref(lease.snapshotRef) || scope.provider || scope.account || scope.generation ||
    scope.attempt || auth !== undefined) fail("Storage source cannot borrow account or foreign root authority");
  const ceilings = { maxEntries: 10000, maxReadBytes: 64 << 20, maxParts: 10000, maxTextBytes: 4 << 20, maxMediaBytes: 8 << 20 };
  keys(lease.limits, Object.keys(ceilings));
  for (const [key, ceiling] of Object.entries(ceilings)) {
    if (!Number.isSafeInteger(lease.limits[key]) || lease.limits[key] < 1 || lease.limits[key] > ceiling) fail("Invalid storage limits");
  }
  if (method === "session.storage.list") {
    if (scope.session || request.locator != null || request.mode || !Number.isInteger(request.limit) ||
      request.limit < 1 || request.limit > 256 || request.cursor && !ref(request.cursor)) fail("Invalid storage enumeration scope");
  } else {
    keys(request.locator, ["rootRef", "sessionId", "key"]);
    if (request.locator.rootRef !== lease.rootRef || !sessionID(request.locator.sessionId) ||
      request.locator.sessionId !== scope.session || !ref(request.locator.key) || request.cursor ||
      request.limit || !["summary", "transcript"].includes(request.mode) || request.mode === "transcript" && !source.transcript) {
      fail("Storage read requires its selected session and root");
    }
  }
  return request;
}

export function validateStorageResult(request, result, stringify) {
  keys(result, ["snapshotRef", "entries", "nextCursor", "sessionId", "mode", "summary", "parts", "media", "cut"]);
  if (!optionalString(result.nextCursor) || !optionalString(result.sessionId) || !optionalString(result.mode) ||
    ["entries", "parts", "media"].some(key => result[key] != null && !Array.isArray(result[key]))) fail("Invalid storage result types");
  if (result.snapshotRef !== request.lease.snapshotRef || typeof result.cut !== "boolean") fail("Invalid storage snapshot or cut flag");
  if (request.locator == null) {
    if (result.mode || result.sessionId || result.summary != null || result.parts?.length || result.media?.length ||
      result.nextCursor && !ref(result.nextCursor) || result.entries !== undefined && !Array.isArray(result.entries) ||
      (result.entries?.length ?? 0) > request.limit) fail("Invalid storage enumeration result");
    const seen = new Set();
    for (const entry of result.entries ?? []) {
      keys(entry, ["locator", "summary"]);
      keys(entry.locator, ["rootRef", "sessionId", "key"]);
      if (entry.locator.rootRef !== request.lease.rootRef || !sessionID(entry.locator.sessionId) ||
        !ref(entry.locator.key) || seen.has(entry.locator.sessionId)) fail("Foreign or duplicate storage session");
      seen.add(entry.locator.sessionId);
      validateSessionSourceResult({ mode: "summary", locator: entry.locator }, {
        mode: "summary", sessionId: entry.locator.sessionId, summary: entry.summary,
      }, stringify);
    }
    return result;
  }
  if (result.sessionId !== request.locator.sessionId || result.mode !== request.mode ||
    result.entries?.length || result.nextCursor) fail("Storage result changed its selected session");
  if (request.mode === "summary") {
    if (result.parts?.length || result.media?.length) fail("Storage summary contains transcript or media");
    validateSessionSourceResult({ mode: "summary", locator: request.locator }, {
      mode: "summary", sessionId: result.sessionId, summary: result.summary,
    }, stringify);
  } else {
    if (result.summary != null) fail("Storage transcript contains summary");
    validateSessionSourceResult({ mode: "transcript", locator: request.locator, limits: request.lease.limits }, {
      mode: "transcript", sessionId: result.sessionId, parts: result.parts ?? [], cut: result.cut,
    }, stringify);
    let total = 0;
    const seen = new Set();
    if (result.media !== undefined && !Array.isArray(result.media)) fail("Invalid storage media");
    for (const media of result.media ?? []) {
      keys(media, ["id", "name", "mime", "bytes"]);
      const bytes = decodeBytes(media.bytes, request.lease.limits.maxMediaBytes);
      total += bytes.length;
      if (!ref(media.id) || seen.has(media.id) || typeof media.mime !== "string" || !media.mime ||
        Buffer.byteLength(media.mime) > 256 || media.name !== undefined && (typeof media.name !== "string" || Buffer.byteLength(media.name) > 1024) ||
        total > request.lease.limits.maxMediaBytes) fail("Invalid or oversized storage media");
      seen.add(media.id);
    }
  }
  return result;
}
