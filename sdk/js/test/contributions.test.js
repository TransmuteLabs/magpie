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

test("Library rules are bounded SDK 1.17 declarations without runtime authority", () => {
  const rule = { id: "style", body: "# Style", paths: ["src/**"] };
  const declaration = { point: "library-rule", version: 1, libraryRule: rule };
  const withRules = {
    ...manifest,
    contract: { ...manifest.contract, version: { major: 1, minor: 17 } },
    contributions: [...manifest.contributions, declaration],
  };
  assert.equal(validateManifest(withRules).contributions.length, 3);
  assert.equal(publicManifest(withRules).contributions, undefined);
  assert.throws(() => validateManifest({ ...withRules, contract: manifest.contract }));
  assert.throws(() => validateManifest({ ...withRules, contributions: [declaration, declaration] }));
  for (const invalid of [{ body: "\u0085" }, { body: "市".repeat(33_334) }, { paths: ["bad\npath"] },
    { paths: ["src/**\0"] }, { paths: Array(51).fill("src/**") }, { agents: ["claude"] }]) {
    assert.throws(() => validateManifest({
      ...withRules, contributions: [{ ...declaration, libraryRule: { ...rule, ...invalid } }],
    }));
  }
});

test("Library commands are bounded SDK 1.18 declarations without file or agent authority", () => {
  const command = { id: "review", description: "市".repeat(1000), prompt: "Review $1" };
  const declaration = { point: "library-command", version: 1, libraryCommand: command };
  const withCommands = {
    ...manifest,
    contract: { ...manifest.contract, version: { major: 1, minor: 18 } },
    compatibility: { hostAPI: "^1.18" },
    contributions: [...manifest.contributions, declaration],
  };
  assert.equal(validateManifest(withCommands).contributions.length, 3);
  assert.equal(publicManifest(withCommands).contributions, undefined);
  assert.throws(() => validateManifest({
    ...withCommands, compatibility: undefined, contract: { ...manifest.contract, version: { major: 1, minor: 17 } },
  }));
  for (const declarations of [[declaration, declaration], Array.from({ length: 17 }, (_, index) => ({
    ...declaration, libraryCommand: { ...command, id: `command-${index}` },
  })), [{ ...declaration, libraryRule: { id: "review", body: "A rule" } }]]) {
    assert.throws(() => validateManifest({ ...withCommands, contributions: declarations }));
  }
  for (const invalid of [{ prompt: "\u0085" }, { prompt: "市".repeat(33_334) }, { prompt: "bad\0prompt" },
    { description: "市".repeat(1001) }, { description: "bad\0description" },
    { agents: ["claude"] }, { taken: {} }, { file: "custom.md" }]) {
    assert.throws(() => validateManifest({
      ...withCommands, contributions: [{ ...declaration, libraryCommand: { ...command, ...invalid } }],
    }));
  }
});
