import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";
import {
  validateManifest, validateSessionRelatedInputs,
} from "../index.js";
import { CONTRACT, validateModule } from "../manifest.js";

function fixture() {
  const capture = (id, body) => {
    const sha = createHash("sha256").update(body).digest("hex"), size = String(Buffer.byteLength(body));
    return {
      path: `sessions/2026/11/15/rollout-${id}.jsonl`, rolloutId: id, encoding: "jsonl",
      rawBytes: size, decodedBytes: size, rawSha256: sha, decodedSha256: sha, prefixSha256: sha,
      byteStart: "0", byteEndExclusive: size, ordinalStart: "0", ordinalEndExclusive: "1",
    };
  };
  return {
    manifest: {
      schema: 1, id: "lineage", version: "1.0.0", runtime: "bun", entry: "worker.js",
      contract: { version: { major: 1, minor: 10 }, capabilities: ["session-migration", "session-related-inputs"] },
      sessions: [{ id: "convert", name: "Convert", from: "codex", to: "omp", outputDir: ".omp/sessions" }],
      compatibility: { hostAPI: "^1.10" },
    },
    request: {
      id: "convert", from: "codex", to: "omp",
      files: [{ path: "selected.jsonl", format: "codex", body: Buffer.from("child\n").toString("base64") }],
      related: {
        version: 1, kind: "codex-rollout-lineage",
        selected: capture("22222222-2222-4222-8222-222222222222", "child\n"),
        ancestors: [{
          body: Buffer.from("prior\n").toString("base64"),
          source: capture("11111111-1111-4111-8111-111111111111", "prior\n"),
        }],
      },
    },
  };
}

test("related captures preserve exact uint64 decimal ranges and source bytes", () => {
  const { manifest, request } = fixture();
  validateManifest(manifest);
  request.related.selected.ordinalStart = "9007199254740993";
  request.related.selected.ordinalEndExclusive = "18446744073709551615";
  const before = JSON.stringify(request);
  validateSessionRelatedInputs(manifest, request);
  assert.equal(JSON.stringify(request), before);
  assert.match(before, /"ordinalStart":"9007199254740993"/);
});

test("related captures require an explicit negotiated migration capability", () => {
  for (const change of [
    value => { value.contract.version.minor = 9; delete value.compatibility; },
    value => { value.contract.version.minor = CONTRACT.version.minor + 1; delete value.compatibility; },
    value => { value.contract.capabilities = ["session-related-inputs"]; },
  ]) {
    const { manifest } = fixture();
    change(manifest);
    assert.throws(() => validateManifest(manifest));
  }
  const { manifest, request } = fixture();
  manifest.contract.capabilities = ["session-migration"];
  assert.throws(() => validateSessionRelatedInputs(manifest, request));
  delete request.related;
  assert.doesNotThrow(() => validateSessionRelatedInputs(manifest, request));
});

test("related captures refuse changed bytes, foreign roots and imprecise ranges", () => {
  for (const change of [
    value => { value.related.version = 2; },
    value => { value.related.kind = "directory"; },
    value => { value.from = "other"; },
    value => { value.files.push(value.files[0]); },
    value => { value.related.selected.path = "../sessions/parent.jsonl"; },
    value => { value.related.selected.path = "credentials/parent.jsonl"; },
    value => { value.related.selected.encoding = "zip"; },
    value => { value.related.selected.rolloutId = "stable-thread-id"; },
    value => { value.related.selected.ordinalStart = 9007199254740993; },
    value => { value.related.selected.ordinalStart = "00"; },
    value => { value.related.selected.ordinalEndExclusive = "18446744073709551616"; },
    value => { value.related.selected.byteEndExclusive = "5"; },
    value => { value.related.selected.prefixSha256 = "a".repeat(64); },
    value => { value.related.selected.rawSha256 = "a".repeat(64); },
    value => { value.related.ancestors[0].body = Buffer.from("other\n").toString("base64"); },
    value => { value.related.ancestors[0].source.rolloutId = value.related.selected.rolloutId; },
    value => { value.related.ancestors[0].source.path = value.related.selected.path; },
    value => { value.related.ancestors = []; },
    value => { value.related.ancestors = Array(32).fill(value.related.ancestors[0]); },
  ]) {
    const { manifest, request } = fixture();
    change(request);
    assert.throws(() => validateSessionRelatedInputs(manifest, request));
  }
});

test("session dispatch rejects unadmitted lineage before invoking the serializer", async () => {
  const { manifest, request } = fixture();
  let called = 0;
  const module = validateModule({
    manifest, handlers: { "session.convert": async (_ctx, call) => { called++; return { files: call.params.files }; } },
  });
  const invoke = params => module.handlers.get("session.convert")({}, { params }, {});
  assert.equal((await invoke(request)).files[0].body, request.files[0].body);
  const changed = structuredClone(request);
  changed.related.ancestors[0].source.byteStart = "1";
  await assert.rejects(invoke(changed));
  assert.equal(called, 1);
  delete changed.related;
  await invoke(changed);
  assert.equal(called, 2);
});
