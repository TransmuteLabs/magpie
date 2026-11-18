import test from "node:test";
import assert from "node:assert/strict";
import { validateManifest, validateModule, requiredMethods } from "../manifest.js";
import { manifest } from "./helpers.js";

function service() {
  const value = manifest(["account-auth", "tool-provider"]);
  value.contract.version.minor = 11;
  value.providers = [{ id: "service", name: "Service", protocols: [] }];
  value.tools = [{ name: "search", inputSchema: { type: "object" } }];
  return value;
}

test("account service needs no invented model or inference capability", () => {
  assert.doesNotThrow(() => validateManifest(service()));
  for (let minor = 0; minor <= 10; minor++) {
    const old = service(); old.contract.version.minor = minor;
    assert.throws(() => validateManifest(old));
  }
  for (const change of [
    value => { value.contract.capabilities = ["tool-provider"]; },
    value => { value.providers[0].models = [{ id: "fabricated" }]; },
    value => { value.providers[0].protocols = ["chat"]; },
    value => { value.contract.capabilities.push("outbound-executor"); },
    value => { value.contract.capabilities.push("model-discovery"); },
  ]) {
    const value = service(); change(value);
    assert.throws(() => validateManifest(value));
  }
});

test("mixed module refuses inference for the service before its handler", async () => {
  const value = service();
  value.contract.capabilities.push("outbound-executor", "model-discovery");
  value.providers.push({ id: "inference", name: "Inference", protocols: ["chat"] });
  let calls = 0;
  const handlers = Object.fromEntries(requiredMethods(value).map(method => [method, async () => { calls++; return null; }]));
  const module = validateModule({ manifest: value, handlers });
  for (const method of ["provider.models", "provider.execute"]) {
    const call = { scope: { provider: "service" }, params: { protocol: "chat", model: "invented" } };
    await assert.rejects(async () => module.handlers.get(method)({}, call, {}));
    assert.equal(calls, 0);
    call.scope.provider = "inference";
    await module.handlers.get(method)({}, call, {});
    assert.equal(calls, 1); calls = 0;
  }
  await module.handlers.get("tool.call")({}, { scope: { provider: "service" }, params: { id: "search", arguments: {} } }, {});
  assert.equal(calls, 1);
});

test("account service search does not promise inference-owned tasks", () => {
  const value = service();
  value.contract.capabilities.push("module-rpc", "usage-reporter");
  value.actions = [
    { id: "query", effect: "read", inputSchema: { type: "object" } },
    { id: "index", effect: "write", inputSchema: { type: "object" } },
  ];
  value.searchSources = [{ id: "search", name: "Search", action: "query", provider: "service" }];
  assert.doesNotThrow(() => validateManifest(value));
  value.providers[0].accountTasks = [{ id: "refresh-index", action: "index" }];
  assert.throws(() => validateManifest(value), "service task lacks an inference parent");
  value.contract.capabilities.push("outbound-executor", "model-discovery");
  value.providers.push({ id: "inference", name: "Inference", protocols: ["chat"] });
  assert.throws(() => validateManifest(value), "inference sibling cannot own this service task");
});
