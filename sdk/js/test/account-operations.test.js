import assert from "node:assert/strict";
import { test } from "node:test";
import { validateManifest, publicManifest } from "../index.js";

function source() {
  return {
    schema: 1, id: "account.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 6 }, capabilities: [
      "inbound-codec", "outbound-executor", "account-auth", "agent-wiring", "module-rpc", "native-account-operations",
    ] },
    inbounds: [{ id: "native", path: "/fixture.Service/Account" }],
    providers: [{ id: "provider", name: "Provider", protocols: ["chat"] }],
    agents: [{ id: "agent", name: "Agent", bin: "agent", fields: [{ key: "model", label: "Model" }],
      launch: { model: "--model" } }],
    actions: [{ id: "read", effect: "read", inputSchema: { type: "object" } }],
    accountOperations: [{ id: "read", agent: "agent", provider: "provider", action: "read" }],
  };
}

test("native account operations require finite owned SDK6 authority and detached snapshots", () => {
  const value = source();
  validateManifest(value);
  const copy = publicManifest(value);
  value.accountOperations[0].agent = "foreign";
  assert.equal(copy.accountOperations[0].agent, "agent");
  for (const change of [
    value => { value.contract.version.minor = 5; },
    value => { value.contract.capabilities.pop(); },
    value => { value.accountOperations = []; },
    value => { value.accountOperations[0].agent = "foreign"; },
    value => { value.accountOperations[0].provider = "foreign"; },
    value => { value.accountOperations[0].action = "foreign"; },
    value => { value.accountOperations.push(value.accountOperations[0]); },
    value => { value.agents[0].launch.model = ""; },
  ]) {
    const changed = source();
    change(changed);
    assert.throws(() => validateManifest(changed));
  }
});

test("native account operation identities use the same module ID grammar as Go", () => {
  for (const identity of ["read.more-v1", "Read", "read more", "read\u00a0more", "read\ufeffmore", "прочитать", "read/more", ""]) {
    const value = source();
    value.accountOperations[0].id = identity;
    if (identity === "read.more-v1") assert.doesNotThrow(() => validateManifest(value));
    else assert.throws(() => validateManifest(value), undefined, identity);
  }
});
