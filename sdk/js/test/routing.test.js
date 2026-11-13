import assert from "node:assert/strict";
import { test } from "node:test";
import { publicManifest, serviceManifest, validateManifest } from "../index.js";
import { validateRoutingRequest, validateRoutingResult } from "../routing.js";

function source() {
  return {
    schema: 1, id: "routing.fixture", version: "1.0.0", runtime: "exec", entry: "worker",
    contract: { version: { major: 1, minor: 4 }, capabilities: ["module-rpc"] },
    actions: [{ id: "rank", effect: "read", inputSchema: { type: "object" } }],
    routingStrategies: [{ id: "reverse", name: "Reverse", action: "rank", flags: { exact: true, byUse: true, byFree: true, byPlan: true, bySpeed: true, byPrice: true } }],
  };
}

test("routing descriptor is root API4 read authority and detached public metadata", () => {
  const value = source();
  assert.equal(validateManifest(value).routingStrategies[0].id, "reverse");
  const publicCopy = publicManifest(value);
  value.routingStrategies[0].flags.exact = false;
  assert.equal(publicCopy.routingStrategies[0].flags.exact, true);
  for (const change of [
    value => { value.contract.version.minor = 3; },
    value => { value.actions[0].effect = "write"; },
    value => { value.routingStrategies[0].action = "missing"; },
    value => { value.routingStrategies[0].id = "other/strategy"; },
    value => { value.routingStrategies[0].timeoutMs = 24; },
    value => { value.routingStrategies[0].timeoutMs = 2001; },
    value => { value.routingStrategies[0].flags.exact = "true"; },
    value => { value.routingStrategies.push(value.routingStrategies[0]); },
    value => { value.routingStrategies = Array(65).fill(value.routingStrategies[0]); },
  ]) {
    const value = source();
    change(value);
    assert.throws(() => validateManifest(value));
  }
});

test("routing DTOs bound metrics, renewal timestamps and complete permutations", () => {
  const request = { strategy: "reverse", pool: "group/example", seats: [{ key: "opaque-a", weight: 1 }, { key: "opaque-b", weight: 2 }] };
  assert.equal(validateRoutingRequest(request), request);
  for (const patch of [
    { weight: 0 }, { key: "private@example.invalid" }, { cost: Infinity }, { tokens: 1e16 },
    { pace: NaN }, { leftIn: "provider\0private" }, { soon: Array(33).fill(1) }, { unreached: 1 },
    { soon: [9007199254740992] }, { account: "true" },
  ]) {
    const value = structuredClone(request);
    Object.assign(value.seats[0], patch);
    assert.throws(() => validateRoutingRequest(value));
  }
  for (const order of [[], [0], [0, 0], [-1, 1], [0, 2], [0, 1, 2]]) {
    assert.throws(() => validateRoutingResult({ order }, 2));
  }
  assert.deepEqual(validateRoutingResult({ order: [1, 0] }, 2), { order: [1, 0] });
});

test("routing public projection hides private ownership without dropping root contributions", () => {
  const root = source();
  root.actions.push({ id: "backend.rank", component: "backend", effect: "read", inputSchema: { type: "object" } });
  root.services = [{
    id: "backend", runtime: "exec", entry: "worker", contract: { id: "routing.backend", version: 1 },
    methods: ["backend.rank"], start: "lazy",
  }];
  root.routingStrategies.push({ id: "private", name: "Private backend", action: "backend.rank" });
  validateManifest(root);
  const view = publicManifest(root);
  assert.doesNotThrow(() => validateManifest({ ...view, runtime: root.runtime, entry: root.entry }));
  assert.deepEqual(view.routingStrategies.map(strategy => strategy.id), ["reverse"]);
  assert.deepEqual(root.routingStrategies.map(strategy => strategy.id), ["reverse", "private"]);
  assert.equal(root.actions[1].component, "backend");
  const service = serviceManifest(root, "backend");
  assert.doesNotThrow(() => validateManifest(service));
  assert.equal(service.routingStrategies, undefined);
  assert.equal(service.actions[0].id, "backend.rank");
});
