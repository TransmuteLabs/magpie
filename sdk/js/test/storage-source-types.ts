import type { Capability, HostMethod, Method, Manifest, StorageReadLease, StorageRequest, StorageSourceDescriptor } from "../index.js";
import { validateStorageSources, validateStorageRequest } from "../index.js";

const capability: Capability = "session-storage-source";
const method: Method = "session.storage.read";
const callback: HostMethod = "storage.native";
const descriptor: StorageSourceDescriptor = {
  id: "history", agent: "fixture", codec: "fixture-db",
  roots: [{ slot: "data", homeRelativePath: ".fixture/data" }],
  families: [{ id: "db", primary: "fixture.db", sidecars: ["fixture.db-wal", "fixture.db-shm"] }],
  transcript: true, resume: "none",
};
const lease: StorageReadLease = {
  sourceId: "history", rootRef: "root", snapshotRef: "snapshot", familyId: "db",
  limits: { maxEntries: 10000, maxReadBytes: 67108864, maxParts: 10000, maxTextBytes: 4194304, maxMediaBytes: 8388608 },
};
const request: StorageRequest = {
  lease, locator: { rootRef: "root", sessionId: "selected", key: "v2:selected" }, mode: "transcript",
};
const manifest: Manifest = {
  schema: 1, id: "fixture.storage", version: "1.0.0", runtime: "native", entry: "plugin.so",
  contract: { version: { major: 1, minor: 8 }, capabilities: [capability] }, storageSources: [descriptor],
};
validateStorageSources(manifest);
validateStorageRequest(manifest, method, { session: "selected" }, undefined, request);
void callback;
