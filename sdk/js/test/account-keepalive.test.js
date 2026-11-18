import assert from "node:assert/strict";
import test from "node:test";
import { publicManifest, serviceManifest, validateManifest } from "../index.js";

const manifest = () => ({
  schema: 1, id: "keepalive.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth"] },
  providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"], keepAlive: { leadMs: 120000 } }],
});

test("idle authentication renewal is a bounded negotiated provider declaration", () => {
  for (const policy of [{}, { leadMs: 60000 }, { leadMs: 3600000 }]) {
    const value = manifest();
    value.providers[0].keepAlive = policy;
    assert.doesNotThrow(() => validateManifest(value));
  }
  for (const policy of [
    false, true, [], 60000, "yes", { leadMs: 0 }, { leadMs: 59999 }, { leadMs: -1 },
    { leadMs: 3600001 }, { leadMs: 1.5 }, { leadMs: NaN }, { leadMs: Infinity },
    { leadMs: "60000" }, { leadMs: null }, { leadMs: 60000, account: "other" },
  ]) {
    const value = manifest();
    value.providers[0].keepAlive = policy;
    assert.throws(() => validateManifest(value), undefined, `unsafe keepAlive admitted: ${JSON.stringify(policy)}`);
  }
  for (const edit of [
    value => { value.contract.version.minor = 7; },
    value => { value.contract.capabilities = ["outbound-executor"]; },
  ]) {
    const value = manifest();
    edit(value);
    assert.throws(() => validateManifest(value));
  }
});

test("absent keepAlive stays off for old SDK and primary snapshots retain independent policy", () => {
  const original = manifest();
  const snapshot = publicManifest(original);
  original.providers[0].keepAlive.leadMs = 60000;
  assert.equal(snapshot.providers[0].keepAlive.leadMs, 120000);
  assert.ok(Object.isFrozen(snapshot.providers[0].keepAlive));
  const root = manifest();
  root.contract.capabilities.push("module-rpc");
  root.services = [{ id: "helper", runtime: "bun", entry: "helper.js", contract: { id: "fixture.helper", version: 1 },
    methods: [], start: "lazy" }];
  const service = serviceManifest(root, "helper");
  assert.equal(service.providers, undefined);
  assert.deepEqual(service.contract.capabilities, ["module-rpc"]);
  for (const policy of [undefined, null]) {
    const old = manifest();
    old.contract.version.minor = 3;
    old.providers[0].keepAlive = policy;
    assert.doesNotThrow(() => validateManifest(old));
  }
});
