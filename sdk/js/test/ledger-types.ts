import type { LedgerObservation, Manifest } from "../index.js";

const manifest: Manifest = {
  schema: 1, id: "ledger", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["ledger-observer"] },
  ledger: { timeoutMs: 250 },
};
const row: LedgerObservation = {
  time: 1, agent: "agent", provider: "vendor", model: "model", input: 0, output: 0,
  cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0, cacheUnknown: true, reasoning: 0,
  millis: 0, ttft: 0, status: 200, rejected: false, handedOn: false, tries: 0,
};
void manifest;
void row;
