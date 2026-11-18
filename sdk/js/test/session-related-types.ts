import {
  type SessionRequest, type SessionInputProvenance, type SessionRelatedInputSet,
  validateSessionRelatedInputs, type Manifest,
} from "../index.js";

const capture: SessionInputProvenance = {
  path: "sessions/parent.jsonl", rolloutId: "11111111-1111-4111-8111-111111111111",
  encoding: "jsonl", rawBytes: "6", rawSha256: "", decodedBytes: "6", decodedSha256: "",
  byteStart: "0", byteEndExclusive: "6", ordinalStart: "9007199254740993",
  ordinalEndExclusive: "18446744073709551615", prefixSha256: "",
};
const related: SessionRelatedInputSet = {
  version: 1, kind: "codex-rollout-lineage", selected: capture, ancestors: [{ body: "", source: capture }],
};
const request: SessionRequest = { id: "convert", from: "codex", to: "omp", files: [], related };
declare const manifest: Manifest;
validateSessionRelatedInputs(manifest, request);
const numeric: SessionInputProvenance = {
  ...capture,
  // @ts-expect-error Lossless provenance ranges use canonical decimal strings.
  ordinalStart: 9007199254740993,
};
void numeric;
