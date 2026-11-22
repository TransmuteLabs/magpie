import { type AccountCheckinResult, type Compatibility, type Contract, type ExecuteRequest, type Manifest, type Model } from "../index.js";

for (const minor of [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20] as const) {
  const contract: Contract = { version: { major: 1, minor }, capabilities: ["module-rpc"] };
  const manifest: Manifest = {
    schema: 1, id: "typed-contract", version: "1.0.0", runtime: "bun", entry: "worker.js", contract,
  };
  void manifest;
}

type AssertFalse<Value extends false> = Value;
type UnsupportedMinorRejected = AssertFalse<21 extends Contract["version"]["minor"] ? true : false>;
type UnsupportedMajorRejected = AssertFalse<2 extends Contract["version"]["major"] ? true : false>;
type UnsupportedHostAPIRejected = AssertFalse<"^1.21" extends Compatibility["hostAPI"] ? true : false>;
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
  // @ts-expect-error SDK API 1.21 is outside the negotiated range.
  version: { major: 1, minor: 21 },
  capabilities: ["session-source"],
};
void sourcePreview;
void legacyContracts;
void future;
const limited: AccountCheckinResult = { outcome: "rate-limited", until: "2026-11-22T16:15:00Z" };
const limitedCompatibility: Compatibility = { hostAPI: "^1.20", nativeABI: 1 };
type MissingCheckinBoundaryRejected = AssertFalse<{ outcome: "rate-limited" } extends AccountCheckinResult ? true : false>;
void limited;
void limitedCompatibility;
const decision: Model = { id: "decision", kind: "decision", protocols: null, operations: ["systemone"] };
const request: ExecuteRequest = { protocol: "", operation: "systemone", model: decision.id,
  caller: { agent: "codex", name: "Codex", via: ["studio", "relay"] } };
void request;
const owner: Manifest = {
  schema: 1, id: "owner", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 16 }, capabilities: ["module-rpc"] },
  actions: [{ id: "read", effect: "read", inputSchema: { type: "object" } }],
  contributions: [{ point: "search", version: 1, search: { id: "web", name: "Web", action: "read" } }],
  settings: [{ name: "enabled", kind: "boolean", title: "Enabled", default: true }],
};
void owner;
