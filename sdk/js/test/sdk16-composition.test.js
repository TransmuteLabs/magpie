import assert from "node:assert/strict";
import { test } from "node:test";
import { publicManifest, requiredMethods, serviceManifest, validateManifest } from "../index.js";

function composite() {
  return {
    schema: 1, id: "sdk16.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 6 }, capabilities: [
      "inbound-codec", "outbound-executor", "account-auth", "agent-wiring", "module-rpc",
      "model-discovery", "native-account-operations", "session-source",
    ] },
    inbounds: [{ id: "native", path: "/fixture.Service/Account" }],
    providers: [{ id: "provider", name: "Provider", protocols: ["chat"] }],
    agents: [{ id: "agent", name: "Agent", bin: "agent", fields: [{ key: "model", label: "Model" }],
      launch: { model: "--model" } }],
    actions: [
      { id: "read", effect: "read", inputSchema: { type: "object" } },
      { id: "helper-read", component: "helper", effect: "read", inputSchema: { type: "object" } },
    ],
    accountOperations: [{ id: "read", agent: "agent", provider: "provider", action: "read" }],
    sessionSources: [{ id: "history", agent: "fixture", codec: "fixture-jsonl-v1", container: "jsonl",
      stores: [{ slot: "main", homeRelativePath: ".fixture/sessions" }],
      layout: { kind: "flat", idFrom: "filename-stem" }, transcript: true, resume: "none" }],
    window: { metadata: "package.json" },
    authBindings: [{ id: "signin", from: "window:page", provider: "provider",
      methods: [{ id: "browser", mode: "poll" }] }],
    services: [{ id: "helper", runtime: "exec", entry: "helper",
      contract: { id: "fixture-helper", version: 1 }, methods: ["helper-read"], start: "lazy" }],
  };
}

test("SDK6 source, native account and window contracts retain their separate authority", () => {
  const value = composite();
  const manifest = validateManifest(value);
  for (const method of ["session.source.read", "inbound.decode", "module.call", "agent.inspect", "auth.start"]) {
    assert.ok(requiredMethods(manifest).includes(method), method);
  }
  const primary = publicManifest(manifest);
  assert.equal(primary.sessionSources.length, 1);
  assert.equal(primary.accountOperations.length, 1);
  assert.deepEqual(primary.actions.map(action => action.id), ["read"]);
  assert.equal(primary.window, undefined);
  assert.equal(primary.authBindings, undefined);
  assert.equal(primary.services, undefined);
  const service = serviceManifest(manifest, "helper");
  assert.deepEqual(service.contract.capabilities, ["module-rpc"]);
  for (const key of ["sessionSources", "accountOperations", "agents", "providers"]) {
    assert.equal(service[key], undefined, key);
  }
  assert.throws(() => { primary.accountOperations[0].agent = "changed"; }, TypeError);
  assert.throws(() => { primary.sessionSources[0].stores[0].homeRelativePath = ".changed"; }, TypeError);
  value.accountOperations[0].agent = "changed";
  value.sessionSources[0].stores[0].homeRelativePath = ".changed";
  assert.equal(manifest.accountOperations[0].agent, "agent");
  assert.equal(manifest.sessionSources[0].stores[0].homeRelativePath, ".fixture/sessions");
  for (const missing of ["session-source", "native-account-operations"]) {
    const value = composite();
    value.contract.capabilities = value.contract.capabilities.filter(capability => capability !== missing);
    assert.throws(() => validateManifest(value), undefined, missing);
  }
  const legacy = composite();
  legacy.contract.version.minor = 5;
  assert.throws(() => validateManifest(legacy));
});
