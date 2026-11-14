import test from "node:test";
import assert from "node:assert/strict";
import { validateManifest, publicManifest } from "../index.js";
import { stringifyJSON } from "../manifest.js";
import { sessionSourceDescendantPath, validateSessionSourceRequest, validateSessionSourceResult } from "../session-source.js";

const manifest = () => ({
  schema: 1, id: "fixture.source", version: "1.0.0", runtime: "native", entry: "plugin.so",
  contract: { version: { major: 1, minor: 9 }, capabilities: ["session-source"] },
  sessionSources: [{
    id: "history", agent: "fixture", codec: "fixture-jsonl-v1", container: "jsonl",
    stores: [{ slot: "main", homeRelativePath: ".fixture/sessions" }],
    layout: { kind: "flat", idFrom: "filename-stem" }, transcript: true, resume: "none",
    descendants: { kind: "journal-stem-jsonl", excludeDirectories: ["btw-history"] },
  }],
});
const request = () => ({
  sourceId: "history", locator: { storeRef: "store", path: "sess.jsonl", sessionId: "sess" },
  snapshot: "a".repeat(64), mode: "summary", descendants: ["sess/child/advisor.jsonl"],
  documents: ["sess.jsonl", "sess/child/advisor.jsonl"].map(path => ({
    path, format: "fixture-jsonl-v1", body: Buffer.from("{}\n").toString("base64"),
  })),
  limits: { maxParts: 100, maxTextBytes: 400000 },
});
const result = () => ({
  mode: "summary", sessionId: "sess", summary: {
    title: "main", cwd: "/work", subs: [{ path: "sess/child/advisor.jsonl", sessionId: "child-id" }],
  },
});

test("source descendant declaration is API9 bounded and immutable", () => {
  const value = manifest(), frozen = validateManifest(value);
  value.sessionSources[0].descendants.excludeDirectories[0] = "changed";
  assert.equal(publicManifest(frozen).sessionSources[0].descendants.excludeDirectories[0], "btw-history");
  for (const mutate of [
    value => { value.contract.version.minor = 8; },
    value => { value.sessionSources[0].descendants.kind = "recursive"; },
    value => { value.sessionSources[0].descendants.excludeDirectories = ["a/b"]; },
    value => { value.sessionSources[0].descendants.excludeDirectories = ["*"]; },
    value => { value.sessionSources[0].descendants.excludeDirectories = ["x", "x"]; },
    value => { value.sessionSources[0].descendants = null; },
    value => { value.sessionSources[0].layout = { kind: "directory", idFrom: "parent-directory", entry: "log.jsonl" }; },
  ]) {
    const invalid = manifest();
    mutate(invalid);
    assert.throws(() => validateManifest(invalid));
  }
});

test("source descendant paths admit only selected stem journals", () => {
  const rule = manifest().sessionSources[0].descendants;
  assert.equal(sessionSourceDescendantPath(rule, "sess.jsonl", "sess/child/advisor.jsonl"), true);
  for (const name of ["sess.jsonl", "sess-other/child.jsonl", "other/child.jsonl",
    "sess/../child.jsonl", "sess/a/btw-history/child.jsonl", "sess/child.txt", "/sess/child.jsonl"]) {
    assert.equal(sessionSourceDescendantPath(rule, "sess.jsonl", name), false, name);
  }
  assert.doesNotThrow(() => validateSessionSourceRequest(manifest(), request(), stringifyJSON));
  for (const mutate of [
    value => { value.documents.pop(); },
    value => { value.descendants.push(value.descendants[0]); },
    value => { value.descendants = ["other/child.jsonl"]; },
    value => { value.descendants = null; },
  ]) {
    const invalid = request();
    mutate(invalid);
    assert.throws(() => validateSessionSourceRequest(manifest(), invalid, stringifyJSON));
  }
});

test("source child IDs account for exactly admitted documents and never borrow main identity", () => {
  assert.doesNotThrow(() => validateSessionSourceResult(request(), result(), stringifyJSON));
  for (const mutate of [
    value => { delete value.summary.subs; },
    value => { value.summary.subs = null; },
    value => { value.summary.subs[0].sessionId = "sess"; },
    value => { value.summary.subs[0].sessionId = "../foreign"; },
    value => { value.summary.subs[0].path = "attachment.jsonl"; },
    value => { value.summary.subs.push(value.summary.subs[0]); },
  ]) {
    const invalid = result();
    mutate(invalid);
    assert.throws(() => validateSessionSourceResult(request(), invalid, stringifyJSON));
  }
  const unadmitted = request();
  delete unadmitted.descendants;
  assert.throws(() => validateSessionSourceResult(unadmitted, result(), stringifyJSON));
});
