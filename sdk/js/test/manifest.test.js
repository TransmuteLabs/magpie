import assert from "node:assert/strict";
import { test } from "node:test";
import { CONTRACT, SDKError, decodeBytes, encodeBytes, MAX_CHUNK_BYTES, publicManifest, requiredMethods, validateManifest } from "../index.js";
import { validateModule } from "../manifest.js";
import { manifest, moduleFor } from "./helpers.js";

test("capabilities require exactly their declared handlers", () => {
  const full = {
    ...manifest(CONTRACT.capabilities.filter(capability => capability !== "module-rpc")),
    inbounds: [{ id: "native", path: "/fixture/native" }],
    providers: [{ id: "sample", name: "Sample", protocols: ["chat", "responses", "messages", "gemini"] }],
    agents: [{ id: "editor", name: "Editor" }],
    sessions: [{ id: "archive", name: "Archive" }],
    tools: [{ name: "echo", inputSchema: { type: "object" } }],
  };
  const methods = requiredMethods(full);
  assert.equal(methods.length, 14);
  const handlers = Object.fromEntries(methods.map(method => [method, async () => null]));
  assert.equal(validateModule({ manifest: full, handlers }).handlers.size, 14);
  for (const method of methods) {
    const missing = { ...handlers };
    delete missing[method];
    assert.throws(() => validateModule({ manifest: full, handlers: missing }), SDKError);
  }
  assert.throws(() => validateModule(moduleFor(async () => null, { handlers: { ...handlers, "sdk.describe": () => null } })), SDKError);
  const inbound = { ...manifest(["inbound-codec", "model-discovery"]), inbounds: full.inbounds };
  assert.deepEqual(requiredMethods(inbound), ["inbound.decode", "inbound.response"]);
});

test("manifest is detached and immutable", () => {
  const source = manifest(), result = validateManifest(source);
  source.contract.capabilities.push("outbound-executor");
  assert.deepEqual(result.contract.capabilities, ["operation-control"]);
  assert.throws(() => result.contract.capabilities.push("account-auth"), TypeError);
});

test("manifest refuses malformed versions, capabilities, identities and entries", () => {
  for (const patch of [
    { schema: 2 }, { id: "../bad" }, { id: "Invalid" }, { version: "1.0" }, { version: "01.0.0" },
    { version: "1.0.0-01" }, { version: "1.0.0+a..b" }, { runtime: "node" }, { entry: "../worker.js" },
    { entry: "/worker.js" }, { entry: "a/../worker.js" }, { entry: "C:\\worker.js" }, { args: ["\0"] },
    { contract: { version: { major: 2, minor: 0 }, capabilities: ["operation-control"] } },
    { contract: { version: { major: 1, minor: 2 }, capabilities: ["operation-control"] } },
    { contract: { version: { major: 1, minor: 0 }, capabilities: [] } },
    { contract: { version: { major: 1, minor: 0 }, capabilities: ["operation-control", "operation-control"] } },
    { contract: { version: { major: 1, minor: 0 }, capabilities: ["unknown"] } },
  ]) assert.throws(() => validateManifest({ ...manifest(), ...patch }), SDKError);
});

test("descriptor identities, paths, protocols, models and schemas are validated", () => {
  const inbound = { ...manifest(["inbound-codec"]), inbounds: [{ id: "native", path: "/fixture/native" }] };
  for (const path of ["/", "/v1/custom", "/api/custom", "/wsl-fixture", "/fixture/%2f", "/fixture/../native", "/fixture//native", "/fixture/"]) {
    assert.throws(() => validateManifest({ ...inbound, inbounds: [{ id: "native", path }] }), SDKError);
  }
  assert.throws(() => validateManifest({ ...inbound, inbounds: [...inbound.inbounds, ...inbound.inbounds] }), SDKError);
  const provider = { ...manifest(["model-discovery"]), providers: [{ id: "sample", name: "Sample", protocols: ["chat"], models: [{ id: "one" }] }] };
  for (const patch of [{ protocols: ["chat", "chat"] }, { protocols: ["unknown"] }, { protocols: ["tool"] }, { models: [{ id: "one" }, { id: "one" }] },
    { models: [{ id: "one", protocols: ["gemini"] }] }, { models: [{ id: "one", maxOutput: -1 }] }]) {
    assert.throws(() => validateManifest({ ...provider, providers: [{ ...provider.providers[0], ...patch }] }), SDKError);
  }
  for (const [capability, key, descriptor] of [
    ["agent-wiring", "agents", { id: "sample", name: "Sample" }],
    ["session-migration", "sessions", { id: "sample", name: "Sample" }],
    ["tool-provider", "tools", { name: "sample", inputSchema: true }],
  ]) {
    assert.throws(() => validateManifest({ ...manifest([capability]), [key]: [descriptor, descriptor] }), SDKError);
    assert.throws(() => validateManifest({ ...manifest(), [key]: [descriptor] }), SDKError);
  }
  for (const inputSchema of [null, [], "schema", 2]) {
    assert.throws(() => validateManifest({ ...manifest(["tool-provider"]), tools: [{ name: "echo", inputSchema }] }), SDKError);
  }
});

test("base64 helpers preserve bytes and enforce exact boundaries", () => {
  const bytes = new Uint8Array([0, 255, 128, 10, 13, 34]);
  assert.deepEqual(decodeBytes(encodeBytes(bytes)), bytes);
  assert.equal(decodeBytes(encodeBytes(new Uint8Array(MAX_CHUNK_BYTES)), MAX_CHUNK_BYTES).length, MAX_CHUNK_BYTES);
  for (const value of ["A", "abc", "A===", "!!!!", "AA=A", "Zh==", "aGVsbG8=\n"]) assert.throws(() => decodeBytes(value), SDKError);
  assert.throws(() => decodeBytes(encodeBytes(new Uint8Array(MAX_CHUNK_BYTES + 1)), MAX_CHUNK_BYTES), SDKError);
  assert.throws(() => encodeBytes("text"), SDKError);
  assert.throws(() => new SDKError("bad code", "secret"), TypeError);
});

test("rich agent descriptors require inspect while plain agents remain manual", () => {
  const plain = { ...manifest(["agent-wiring"]), agents: [{ id: "editor", name: "Editor" }] };
  const plan = async () => ({ files: [] }), inspect = async () => ({ connected: false, values: {} });
  assert.deepEqual(requiredMethods(plain), ["agent.plan"]);
  assert.doesNotThrow(() => validateModule({ manifest: plain, handlers: { "agent.plan": plan } }));
  assert.throws(() => validateModule({ manifest: plain, handlers: { "agent.plan": plan, "agent.inspect": inspect } }), SDKError);
  for (const metadata of [
    { configFiles: [".config/editor/settings.json"] },
    { bin: "editor", configFiles: [".config/editor/settings.json"] },
    { configFiles: [".config/editor/settings.json"], fields: [{ key: "model", label: "Model" }] },
  ]) {
    const rich = { ...plain, agents: [{ ...plain.agents[0], ...metadata }] };
    assert.deepEqual(requiredMethods(rich), ["agent.inspect", "agent.plan"]);
    assert.throws(() => validateModule({ manifest: rich, handlers: { "agent.plan": plan } }), SDKError);
    assert.doesNotThrow(() => validateModule({ manifest: rich, handlers: { "agent.plan": plan, "agent.inspect": inspect } }));
  }
});

test("agent metadata presence matches Go plain, binary, config and fields vectors", () => {
  for (const descriptor of [
    { id: "plain", name: "Plain" },
    { id: "binary", name: "Binary", bin: "editor" },
    { id: "config", name: "Config", configFiles: [".editor/config.json"] },
    { id: "fields", name: "Fields", fields: [{ key: "model", label: "Model" }] },
  ]) {
    const source = { ...manifest(["agent-wiring"]), agents: [descriptor] };
    assert.doesNotThrow(() => validateManifest(source), descriptor.id);
    const rich = !!(descriptor.bin || descriptor.configFiles?.length || descriptor.fields?.length);
    assert.deepEqual(requiredMethods(source), rich ? ["agent.inspect", "agent.plan"] : ["agent.plan"]);
    const handlers = { "agent.plan": async () => ({ files: [] }) };
    if (rich) {
      assert.throws(() => validateModule({ manifest: source, handlers }), SDKError);
      handlers["agent.inspect"] = async () => ({ connected: false, values: {} });
    }
    assert.doesNotThrow(() => validateModule({ manifest: source, handlers }), descriptor.id);
  }
});

test("rich agent binary, fields and config paths match Go descriptor validation", () => {
  const base = {
    ...manifest(["agent-wiring"]),
    agents: [{ id: "editor", name: "Editor", bin: "editor", configFiles: [".config/editor/settings.json"], fields: [{ key: "model", label: "Model" }] }],
  };
  for (const patch of [
    { bin: "/usr/bin/editor" }, { bin: "Editor" }, { bin: "editor.exe/path" },
    { bin: "a".repeat(129) }, { bin: false },
    { configFiles: false }, { configFiles: ["Config.json", "config.json"] },
    { configFiles: Array.from({ length: 129 }, (_, index) => `.config/editor/${index}.json`) },
    { fields: false }, { fields: [{ key: "model", label: "" }] },
    { fields: [{ key: "model", label: "Model" }, { key: "model", label: "Other" }] },
    { fields: [{ key: "INVALID", label: "Model" }] }, { fields: [{ key: "model", label: "\n" }] },
    { fields: [{ key: "model", label: "x".repeat(257) }] },
    { fields: Array.from({ length: 129 }, (_, index) => ({ key: `field-${index}`, label: "Field" })) },
  ]) assert.throws(() => validateManifest({ ...base, agents: [{ ...base.agents[0], ...patch }] }), SDKError);
  assert.doesNotThrow(() => validateManifest({ ...base, agents: [{ ...base.agents[0], configFiles: [] }] }));
  assert.doesNotThrow(() => validateManifest({ ...manifest(["agent-wiring"]), agents: [{ id: "editor", name: "Editor", bin: "editor" }] }));
  assert.doesNotThrow(() => validateManifest({ ...manifest(["agent-wiring"]), agents: [{ id: "editor", name: "Editor", fields: [{ key: "model", label: "Model" }] }] }));
  const boundary = { ...base.agents[0], configFiles: Array.from({ length: 128 }, (_, index) => `.config/editor/${index}.json`),
    fields: Array.from({ length: 128 }, (_, index) => ({ key: `field-${index}`, label: "Field" })) };
  assert.doesNotThrow(() => validateManifest({ ...base, agents: [boundary] }));
});

test("agent and session locations are exact bounded paths beneath HOME", () => {
  const agent = value => ({ ...manifest(["agent-wiring"]), agents: [{ id: "editor", name: "Editor", configFiles: [value] }] });
  const session = value => ({ ...manifest(["session-migration"]), sessions: [{ id: "convert", name: "Convert", from: "source", to: "target", outputDir: value }] });
  for (const path of ["", ".", "..", "/", "/tmp/config", "../config", "a/../config", "a//config", "a/", "C:/config",
    "a\\config", "a\0config", "a\nconfig", "*.json", "a?.json", "[a].json", ".MAGPIE-EXTENSION/state", "a/.magpie-extension-state",
    "a".repeat(1025), "é".repeat(513)]) {
    assert.throws(() => validateManifest(agent(path)), SDKError, "agent " + JSON.stringify(path));
    assert.throws(() => validateManifest(session(path)), SDKError, "session " + JSON.stringify(path));
  }
  for (const path of [".config/editor/settings.json", "Application Support/editor/config.json", "a".repeat(1024), "é".repeat(512)]) {
    assert.doesNotThrow(() => validateManifest(agent(path)));
    assert.doesNotThrow(() => validateManifest(session(path)));
  }
});

test("session routing metadata is either absent or a complete distinct format route", () => {
  const plain = { ...manifest(["session-migration"]), sessions: [{ id: "convert", name: "Convert" }] };
  assert.doesNotThrow(() => validateManifest(plain));
  for (const metadata of [
    { from: "source" }, { to: "target" }, { outputDir: ".config/converted" },
    { from: "same", to: "same", outputDir: ".config/converted" },
    { from: "Source", to: "target", outputDir: ".config/converted" },
    { from: "source", to: false, outputDir: ".config/converted" },
  ]) assert.throws(() => validateManifest({ ...plain, sessions: [{ ...plain.sessions[0], ...metadata }] }), SDKError);
  const rich = { ...plain, sessions: [{ ...plain.sessions[0], from: "source", to: "target", outputDir: ".config/converted" }] };
  assert.doesNotThrow(() => validateManifest(rich));
  assert.deepEqual(requiredMethods(rich), ["session.convert"]);
});

test("advertised provider, agent and session capabilities require their descriptors", () => {
  for (const capability of ["outbound-executor", "account-auth", "usage-reporter", "agent-wiring", "session-migration"]) {
    assert.throws(() => validateManifest(manifest([capability])), SDKError, capability);
  }
  const provider = { id: "sample", name: "Sample", protocols: ["chat"] };
  for (const capability of ["outbound-executor", "account-auth", "usage-reporter"]) {
    assert.doesNotThrow(() => validateManifest({ ...manifest([...new Set([capability, "outbound-executor"])]), providers: [provider] }));
  }
  assert.doesNotThrow(() => validateManifest({
    ...manifest(["inbound-codec", "model-discovery"]), inbounds: [{ id: "native", path: "/fixture/native" }],
  }));
});

test("rich nested metadata is cloned, frozen and preserved by the public manifest", () => {
  const source = {
    ...manifest(["agent-wiring", "session-migration"]),
    agents: [{ id: "editor", name: "Editor", bin: "editor", configFiles: [".config/editor/settings.json"], fields: [{ key: "model", label: "Model" }] }],
    sessions: [{ id: "convert", name: "Convert", from: "source", to: "target", outputDir: ".config/converted" }],
  };
  const snapshot = validateManifest(source), published = publicManifest(snapshot);
  source.agents[0].configFiles[0] = "../outside";
  source.agents[0].fields[0].label = "Changed";
  source.sessions[0].outputDir = "../outside";
  assert.equal(published.agents[0].configFiles[0], ".config/editor/settings.json");
  assert.equal(published.agents[0].fields[0].label, "Model");
  assert.equal(published.sessions[0].outputDir, ".config/converted");
  assert.equal(published.entry, undefined);
  assert.throws(() => { published.agents[0].fields[0].label = "Mutated"; }, TypeError);
  assert.throws(() => published.agents[0].configFiles.push("outside"), TypeError);
});
