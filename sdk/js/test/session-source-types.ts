import type { Manifest, SessionSourceReadRequest, SessionSourceReadResult } from "../index.js";
import { validateSessionSourceRequest } from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "fixture", version: "1.0.0", runtime: "native", entry: "plugin.so",
  contract: { version: { major: 1, minor: 6 }, capabilities: ["session-source"] },
  sessionSources: [{ id: "history", agent: "fixture", codec: "fixture-jsonl", container: "jsonl",
    stores: [{ slot: "main", homeRelativePath: ".fixture/sessions" }],
    layout: { kind: "flat", idFrom: "filename-stem" }, transcript: true, resume: "none" }],
};
const request: SessionSourceReadRequest = {
  sourceId: "history", locator: { storeRef: "store", path: "sess.jsonl", sessionId: "sess" },
  snapshot: "a".repeat(64), mode: "summary", documents: [], limits: { maxParts: 1, maxTextBytes: 10 },
};
const result: SessionSourceReadResult = { mode: "summary", sessionId: "sess", summary: { title: "", cwd: "" } };
validateSessionSourceRequest(manifest, request, JSON.stringify);
void result;
