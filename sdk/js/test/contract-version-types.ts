import { type Compatibility, type Contract, type ExecuteRequest, type Manifest, type Model } from "../index.js";

for (const minor of [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16] as const) {
  const contract: Contract = { version: { major: 1, minor }, capabilities: ["module-rpc"] };
  const manifest: Manifest = {
    schema: 1, id: "typed-contract", version: "1.0.0", runtime: "bun", entry: "worker.js", contract,
  };
  void manifest;
}

type AssertFalse<Value extends false> = Value;
type UnsupportedMinorRejected = AssertFalse<17 extends Contract["version"]["minor"] ? true : false>;
type UnsupportedMajorRejected = AssertFalse<2 extends Contract["version"]["major"] ? true : false>;
type UnsupportedHostAPIRejected = AssertFalse<"^1.17" extends Compatibility["hostAPI"] ? true : false>;
const compatibility: Compatibility = { hostAPI: "^1.8", nativeABI: 1 };
void compatibility;
const decisionCompatibility: Compatibility = { hostAPI: "^1.7", nativeABI: 1 };
void decisionCompatibility;
const sourcePreview: Contract = { version: { major: 1, minor: 6 }, capabilities: ["session-source"] };
const legacyContracts: Contract[] = [
  { version: { major: 1, minor: 0 }, capabilities: ["session-migration"] },
  { version: { major: 1, minor: 1 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 2 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 3 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 4 }, capabilities: ["module-rpc"] },
  { version: { major: 1, minor: 5 }, capabilities: ["module-rpc"] },
];
const future: Contract = {
  // @ts-expect-error SDK API 1.17 is outside the negotiated range.
  version: { major: 1, minor: 17 },
  capabilities: ["session-source"],
};
void sourcePreview;
void legacyContracts;
void future;
const decision: Model = { id: "decision", kind: "decision", protocols: null, operations: ["systemone"] };
const request: ExecuteRequest = { protocol: "", operation: "systemone", model: decision.id };
void request;
const owner: Manifest = {
  schema: 1, id: "owner", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 16 }, capabilities: ["module-rpc"] },
  actions: [{ id: "read", effect: "read", inputSchema: { type: "object" } }],
  contributions: [{ point: "search", version: 1, search: { id: "web", name: "Web", action: "read" } }],
  settings: [{ name: "enabled", kind: "boolean", title: "Enabled", default: true }],
};
void owner;
