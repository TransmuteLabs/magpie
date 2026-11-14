import { type AccountCheckinRequest, type AccountCheckinResult, type Manifest, validateAccountCheckinRequest, validateAccountCheckinResult } from "../index.js";

const request: AccountCheckinRequest = validateAccountCheckinRequest({ day: "2026-11-14", proxy: "direct" });
const result: AccountCheckinResult = validateAccountCheckinResult({ outcome: "claimed", credit: 0 });
const manifest: Manifest = {
  schema: 1, id: "checkin.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth", "module-rpc"] },
  activation: ["gateway"],
  providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"], checkin: { action: "daily.claim", recheck: true } }],
};
// @ts-expect-error gateway activation is finite
manifest.activation = ["background"];
// @ts-expect-error outcome is finite
const invalid: AccountCheckinResult = { outcome: "invented" };
void request;
void result;
void manifest;
void invalid;
