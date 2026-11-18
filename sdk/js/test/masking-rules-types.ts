import type { Manifest, MaskingRuleSet } from "../index.js";

const maskingRuleSets: readonly MaskingRuleSet[] = [
  { id: "team", rules: [{ kind: "SDK_TICKET", regex: "ticket-[0-9]{8}" }] },
];
const manifest: Manifest = {
  schema: 1, id: "mask", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 15 }, capabilities: ["masking-rules"] },
  maskingRuleSets,
};
void manifest;
