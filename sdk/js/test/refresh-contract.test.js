import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, SDKError } from "../index.js";

test("transactional refresh requires API 1.3 while old auth modules remain valid", () => {
  const manifest = {
    schema: 1, id: "refresh", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 3 }, capabilities: ["account-auth", "outbound-executor"] },
    providers: [{ id: "vendor", name: "Vendor", protocols: ["chat"] }],
    compatibility: { hostAPI: "^1.3" },
  };
  assert.doesNotThrow(() => validateManifest(manifest));
  manifest.contract.version.minor = 2;
  assert.throws(() => validateManifest(manifest), SDKError);
  delete manifest.compatibility;
  assert.doesNotThrow(() => validateManifest(manifest));
});
