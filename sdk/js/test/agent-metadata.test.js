import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, validateAgentBindingSnapshot, validateAgentBindingChange } from "../index.js";

function fixture() {
  return {
    schema: 1, id: "fixture-agent", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 5 }, capabilities: ["agent-wiring"] },
    agents: [{
      id: "native-profile", name: "Native profile", bin: "fixture-editor",
      fields: [{ key: "model", label: "Model" }],
      aliases: ["native-profile-cli"], ua: ["fixture-agent/"],
      launch: { env: { FIXTURE_URL: "{url}" }, args: { "--mode": "native" }, model: "--model", plain: ["login"] },
      profile: { target: "fixture-base" },
      library: { instructions: ".fixture/AGENTS.md", mcp: ".fixture/mcp.json", mcpFormat: "claude", skills: ".fixture/skills" },
    }],
  };
}

test("agent metadata requires API 1.5 and snapshots nested declarations", () => {
  const value = fixture();
  const frozen = validateManifest(value);
  value.agents[0].launch.env.FIXTURE_URL = "changed";
  value.agents[0].profile.target = "changed";
  value.agents[0].aliases[0] = "changed";
  assert.equal(frozen.agents[0].launch.env.FIXTURE_URL, "{url}");
  assert.equal(frozen.agents[0].profile.target, "fixture-base");
  assert.equal(frozen.agents[0].aliases[0], "native-profile-cli");
  const legacy = fixture();
  legacy.contract.version.minor = 4;
  assert.throws(() => validateManifest(legacy));
  legacy.agents = [{ id: "editor", name: "Editor" }];
  assert.doesNotThrow(() => validateManifest(legacy));
});

test("agent binding snapshots retain uint64 string precision and deny scope/value overrides", () => {
  const manifest = fixture(), descriptor = manifest.agents[0];
  const before = { target: descriptor.profile.target, revision: "18446744073709551615", connected: true, values: { model: "magpie/model" } };
  assert.doesNotThrow(() => validateAgentBindingSnapshot(manifest, descriptor, before));
  const change = { revision: before.revision, connected: true, values: { model: "magpie/next" } };
  assert.doesNotThrow(() => validateAgentBindingChange(manifest, descriptor, before, "set", change));
  for (const value of [
    { ...before, target: "another" },
    { ...before, revision: "01" },
    { ...before, revision: "18446744073709551616" },
    { ...before, values: { credentials: "not a field" } },
    { ...before, values: null },
  ]) assert.throws(() => validateAgentBindingSnapshot(manifest, descriptor, value));
  assert.throws(() => validateAgentBindingChange(manifest, descriptor, before, "set", { ...change, revision: "0" }));
  assert.throws(() => validateAgentBindingChange(manifest, descriptor, before, "set", { ...change, target: "another" }));
  assert.throws(() => validateAgentBindingChange(manifest, descriptor, before, "set", { ...change, values: null }));
  assert.throws(() => validateAgentBindingChange(manifest, descriptor, before, "set", { ...change, values: {} }));
});

test("agent metadata rejects identity collisions, invalid launch and escaping library paths", () => {
  for (const change of [
    value => { value.agents[0].profile.target = "native-profile"; },
    value => { value.agents[0].aliases = ["same", "same"]; },
    value => { value.agents.push({ id: "native-profile-cli", name: "Another" }); },
    value => { value.agents[0].launch.env["BAD-NAME"] = "x"; },
    value => { value.agents[0].launch.model = "--model;command"; },
    value => { value.agents[0].launch.plain = ["*"]; },
    value => { value.agents[0].library.skills = "../outside"; },
  ]) {
    const value = fixture();
    change(value);
    assert.throws(() => validateManifest(value));
  }
});

test("agent environment config placeholder requires a declared config source", () => {
  const value = fixture();
  delete value.agents[0].profile;
  delete value.agents[0].configFiles;
  value.agents[0].launch.env.CONFIG = "{config}";
  assert.throws(() => validateManifest(value));
  const file = structuredClone(value);
  file.agents[0].configFiles = ["config with spaces.json"];
  assert.doesNotThrow(() => validateManifest(file));
  const profile = structuredClone(value);
  profile.agents[0].profile = { target: "base" };
  assert.doesNotThrow(() => validateManifest(profile));
});
