import test from "node:test";
import assert from "node:assert/strict";
import { storageFamilyAllows, validateStorageSources, validateStorageRequest, validateStorageResult } from "../storage-source.js";
import { requiredMethods, stringifyJSON, validateManifest } from "../manifest.js";
import { harness, moduleFor } from "./helpers.js";

const manifest = () => ({
  schema: 1, id: "storage-fixture", version: "1.0.0", entry: "module.so",
  runtime: "native", contract: { version: { major: 1, minor: 8 }, capabilities: ["session-storage-source"] },
  storageSources: [{
    id: "history", agent: "fixture", codec: "fixture-db", roots: [{ slot: "data", homeRelativePath: ".fixture/data" }],
    families: [{ id: "db", primary: "fixture.db", sidecars: ["fixture.db-wal", "fixture.db-shm"], trees: ["legacy/session"] }],
    transcript: true, resume: "none",
  }],
});
const request = () => ({
  lease: { sourceId: "history", rootRef: "root", snapshotRef: "snapshot", familyId: "db",
    limits: { maxEntries: 10000, maxReadBytes: 64 << 20, maxParts: 10000, maxTextBytes: 4 << 20, maxMediaBytes: 8 << 20 } },
  locator: { rootRef: "root", sessionId: "selected", key: "v2:selected" }, mode: "transcript",
});
const result = () => ({
  snapshotRef: "snapshot", sessionId: "selected", mode: "transcript", cut: false,
  parts: [{ role: "user", kind: "text", text: "hello" }],
  media: [{ id: "media:one", mime: "image/png", bytes: Buffer.from("png").toString("base64") }],
});

test("ordinary manifests negotiate finite native storage at API 1.8", () => {
  const value = validateManifest(manifest());
  assert.deepEqual(requiredMethods(value), ["session.storage.list", "session.storage.read"]);
  const older = manifest();
  older.contract.version.minor = 7;
  assert.throws(() => validateManifest(older));
});

test("storage authoring keeps finite native-only roots and literal home names", () => {
  validateStorageSources(manifest());
  const literal = manifest();
  literal.storageSources[0].roots[0].homeRelativePath = ".fixture/$literal~";
  validateStorageSources(literal);
  for (const mutate of [
    value => { value.runtime = "bun"; },
    value => { value.contract.version.minor = 7; },
    value => { value.storageSources[0].roots[0].homeRelativePath = "../outside"; },
    value => { value.storageSources[0].families[0].trees[0] = "legacy/session/"; },
    value => { value.storageSources[0].roots[0].nativeRootSlot = 0; },
    value => { delete value.storageSources[0].transcript; },
    value => { value.storageSources[0].families[0].sidecars.push("fixture.db-wal"); },
  ]) {
    const value = manifest();
    mutate(value);
    assert.throws(() => validateStorageSources(value));
  }
  const family = manifest().storageSources[0].families[0];
  for (const name of ["fixture.db", "fixture.db-wal", "legacy/session/a.json"]) assert.equal(storageFamilyAllows(family, name), true);
  for (const name of ["../fixture.db", "fixture.db-journal", "legacy/other/a.json", "legacy/session/"]) assert.equal(storageFamilyAllows(family, name), false);
});

test("storage calls require exact selected session and empty account authority", () => {
  validateStorageRequest(manifest(), "session.storage.read", { session: "selected" }, undefined, request());
  for (const scope of [{ session: "foreign" }, { session: "selected", provider: "vendor" },
    { session: "selected", account: "account" }, { session: "selected", generation: 1 }, { session: "selected", attempt: 1 }]) {
    assert.throws(() => validateStorageRequest(manifest(), "session.storage.read", scope, undefined, request()));
  }
  assert.throws(() => validateStorageRequest(manifest(), "session.storage.read", { session: "selected" }, { key: "secret" }, request()));
  assert.throws(() => validateStorageRequest(manifest(), "session.storage.read", { session: "selected" }, null, request()));
  for (const mutate of [
    value => { value.locator.rootRef = "foreign"; },
    value => { value.lease.limits.maxReadBytes++; },
    value => { value.cursor = 0; },
    value => { value.rawSQL = "SELECT *"; },
  ]) {
    const value = request();
    mutate(value);
    assert.throws(() => validateStorageRequest(manifest(), "session.storage.read", { session: "selected" }, undefined, value));
  }
});

test("storage publication preserves typed media, session, snapshot and explicit cut", () => {
  validateStorageResult(request(), result(), stringifyJSON);
  for (const mutate of [
    value => { value.sessionId = "foreign"; },
    value => { value.snapshotRef = "foreign"; },
    value => { delete value.cut; },
    value => { value.media.push(value.media[0]); },
    value => { value.parts = {}; },
    value => { value.nextCursor = 0; },
    value => { value.rawPath = "/etc/passwd"; },
  ]) {
    const value = result();
    mutate(value);
    assert.throws(() => validateStorageResult(request(), value, stringifyJSON));
  }
  const list = { lease: request().lease, limit: 2 };
  for (const invalid of [{ parts: {} }, { media: {} }, { entries: {} }]) {
    assert.throws(() => validateStorageResult(list, { snapshotRef: "snapshot", cut: false, ...invalid }, stringifyJSON));
  }
});

test("an ordinary worker cannot borrow storage callbacks from an unrelated parent", async () => {
  const module = moduleFor((_context, _call, stream) => stream.hostCall("storage.native", { lease: request().lease }));
  module.manifest.contract.version.minor = 8;
  const worker = harness(module);
  try {
    worker.send({ kind: "call", id: "configure", method: "component.configure",
      params: { component: "primary", instance: "instance", contract: {}, config: { version: "1", values: {} } } });
    assert.equal((await worker.received.next()).error, undefined);
    worker.send({ kind: "call", id: "borrow", method: "operation.status", scope: {} });
    const response = await worker.received.next();
    assert.equal(response.kind, "result");
    assert.equal(response.error?.code, "invalid");
    assert.equal(response.error?.message, "Storage callbacks require their selected source parent");
  } finally { await worker.close(); }
});
