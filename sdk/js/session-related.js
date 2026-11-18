import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import { decodeBytes, SDKError } from "./bytes.js";
import { sessionSourcePath } from "./session-source.js";

const fail = message => { throw new SDKError("invalid_params", message); };
const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
const hash = body => createHash("sha256").update(body).digest("hex");
const digest = value => typeof value === "string" && /^[0-9a-f]{64}$/.test(value);
const rollout = value => typeof value === "string" &&
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value);
const budget = 16 * 1024 * 1024;

function decimal(value) {
  if (typeof value !== "string" || !/^(0|[1-9][0-9]{0,19})$/.test(value)) {
    fail("Related input ranges require canonical decimal uint64 strings");
  }
  const number = BigInt(value);
  if (number > 18446744073709551615n) fail("Related input range overflows uint64");
  return number;
}

function provenance(source, encoded, selected) {
  if (!object(source) || !sessionSourcePath(source.path) ||
    !source.path.startsWith("sessions/") && !source.path.startsWith("archived_sessions/") ||
    !rollout(source.rolloutId) || !["jsonl", "jsonl.zst"].includes(source.encoding) ||
    !source.path.endsWith(`.${source.encoding}`)) fail("Invalid related input capture identity");
  const body = Buffer.from(decodeBytes(encoded, budget));
  const [raw, decoded, start, end, first, last] = [
    source.rawBytes, source.decodedBytes, source.byteStart, source.byteEndExclusive,
    source.ordinalStart, source.ordinalEndExclusive,
  ].map(decimal);
  if (raw === 0n || raw > BigInt(budget) || decoded === 0n || decoded > BigInt(budget) ||
    start !== 0n || end === 0n || end > decoded || end !== BigInt(body.length) ||
    first >= last || selected && end !== decoded) fail("Invalid related input prefix or ordinal range");
  if (![source.rawSha256, source.decodedSha256, source.prefixSha256].every(digest) ||
    source.prefixSha256 !== hash(body) ||
    end === decoded && source.prefixSha256 !== source.decodedSha256 ||
    source.encoding === "jsonl" && (raw !== decoded || source.rawSha256 !== source.decodedSha256)) {
    fail("Related input body differs from its captured digest");
  }
  return { bytes: body.length, raw, decoded };
}

export function validateSessionRelatedContract(manifest) {
  if (!manifest.contract.capabilities.includes("session-related-inputs")) return;
  if (manifest.contract.version.major !== 1 || manifest.contract.version.minor < 10 ||
    !manifest.contract.capabilities.includes("session-migration")) {
    fail("session-related-inputs requires API 1.10 and session-migration");
  }
}

export function validateSessionRelatedInputs(manifest, request) {
  if (request?.related == null) return;
  validateSessionRelatedContract(manifest);
  if (!manifest.contract.capabilities.includes("session-related-inputs")) fail("session-related-inputs capability is required");
  const related = request.related;
  if (!object(related) || related.version !== 1 || related.kind !== "codex-rollout-lineage" ||
    request.from !== "codex" || !Array.isArray(request.files) || request.files.length !== 1 ||
    !Array.isArray(related.ancestors) || related.ancestors.length === 0 || related.ancestors.length >= 32) {
    fail("Invalid finite codex-rollout-lineage carrier");
  }
  const totals = provenance(related.selected, request.files[0]?.body, true);
  const ids = new Set([related.selected.rolloutId]), paths = new Set([related.selected.path]);
  for (const ancestor of related.ancestors) {
    if (!object(ancestor) || !object(ancestor.source) ||
      ids.has(ancestor.source.rolloutId) || paths.has(ancestor.source.path)) fail("Duplicate related input identity or capture path");
    const size = provenance(ancestor.source, ancestor.body, false);
    ids.add(ancestor.source.rolloutId);
    paths.add(ancestor.source.path);
    totals.bytes += size.bytes;
    totals.raw += size.raw;
    totals.decoded += size.decoded;
    if (totals.bytes > budget || totals.raw > BigInt(budget) || totals.decoded > BigInt(budget)) {
      fail("Related session inputs exceed their byte budget");
    }
  }
}
