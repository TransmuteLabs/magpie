import assert from "node:assert/strict";
import test from "node:test";
import { publicManifest, validateManifest } from "../index.js";

const descriptor = { id: "fixture", name: "Fixture", action: "lookup" };
const manifest = {
  schema: 1, id: "owner-fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 16 }, capabilities: ["module-rpc"] },
  actions: [{ id: "lookup", effect: "read", inputSchema: { type: "object" } }],
  contributions: [
    { point: "routing", version: 1, routing: descriptor },
    { point: "search", version: 1, search: descriptor },
  ],
  settings: [
    { name: "enabled", kind: "boolean", title: "Enabled", default: false },
    { name: "length", kind: "integer", title: "Length", default: 2, minimum: 1, maximum: 5, enum: [2, 3] },
  ],
};

test("versioned owner registrations and settings stay in the installation manifest", () => {
  const checked = validateManifest(manifest);
  assert.equal(checked.contributions.length, 2);
  assert.equal(checked.settings.length, 2);
  const handshake = publicManifest(checked);
  assert.equal(handshake.contributions, undefined);
  assert.equal(handshake.settings, undefined);
  const invalid = [
    { contributions: [{ point: "unknown", version: 1 }] },
    { contributions: [{ point: "search", version: 2, search: descriptor }] },
    { contributions: [{ point: "search", version: 1, search: descriptor, routing: descriptor }] },
    { searchSources: [descriptor] },
    { actions: [{ ...manifest.actions[0], effect: "write" }] },
    { contract: { ...manifest.contract, version: { major: 1, minor: 15 } } },
    { settings: [{ ...manifest.settings[1], default: "2" }] },
    { settings: [{ ...manifest.settings[1], enum: [2, 2] }] },
    { settings: [{ ...manifest.settings[1], enum: [3] }] },
    { settings: [{ ...manifest.settings[1], maximum: null }] },
    { settings: [{ ...manifest.settings[0], default: null }] },
    { settings: [manifest.settings[0], manifest.settings[0]] },
  ];
  for (const conflict of invalid) assert.throws(() => validateManifest({ ...manifest, ...conflict }));
});
