import {
  appendOwnCall, type Manifest, type OwnCall, type OwnCallResult,
  type OutsideCallsRequest, type OutsideCallsResult, type Stream, validateOutsideCallsResult,
} from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth", "usage-reporter"] },
  providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"], calls: true }],
};
const call: OwnCall = { kind: "cache_keepalive", model: "model", usage: { facts: { inputTokens: 0 } } };
const request: OutsideCallsRequest = { since: 1700000000000, until: 1700000060000 };
const result: OutsideCallsResult = { calls: [{ id: "vendor-id", time: request.since, model: "model" }] };
validateOutsideCallsResult(manifest, request, result);
declare const stream: Stream;
const receipt: Promise<OwnCallResult> = appendOwnCall(stream, call);
void receipt;
