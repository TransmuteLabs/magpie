import assert from "node:assert/strict";
import test from "node:test";
import { publicManifest, serviceManifest, validateManifest, validateAccountCheckinRequest, validateAccountCheckinResult } from "../index.js";

const manifest = () => ({
  schema: 1, id: "checkin.fixture", version: "1.0.0", runtime: "bun", entry: "worker.js",
  contract: { version: { major: 1, minor: 8 }, capabilities: ["outbound-executor", "account-auth", "module-rpc"] },
  providers: [{ id: "vendor", name: "Fixture", protocols: ["chat"], checkin: { action: "daily.claim", recheck: true } }],
  actions: [{ id: "daily.claim", effect: "write", inputSchema: { type: "object" } }],
  activation: ["gateway"],
});

test("account check-in and gateway activation are finite negotiated declarations", () => {
  const original = manifest(), snapshot = publicManifest(original);
  original.providers[0].checkin.action = "other";
  original.activation[0] = "other";
  assert.equal(snapshot.providers[0].checkin.action, "daily.claim");
  assert.deepEqual(snapshot.activation, ["gateway"]);
  assert.doesNotThrow(() => validateManifest(manifest()));
  for (const edit of [
    value => { value.contract.version.minor = 7; },
    value => { value.providers[0].checkin.action = "other"; },
    value => { value.actions[0].effect = "read"; },
    value => { value.providers[0].checkin.recheck = "yes"; },
    value => { value.contract.capabilities = ["outbound-executor", "module-rpc"]; },
    value => { value.activation = ["gateway", "gateway"]; },
    value => { value.activation = ["background"]; },
    value => { value.activation = "gateway"; },
  ]) {
    const value = manifest();
    edit(value);
    assert.throws(() => validateManifest(value));
  }
  const root = manifest();
  root.services = [{ id: "helper", runtime: "bun", entry: "helper.js", contract: { id: "checkin.helper", version: 1 },
    methods: ["daily.claim"], start: "lazy" }];
  root.actions[0].component = "helper";
  assert.equal(serviceManifest(root, "helper").activation, undefined);
  assert.equal(publicManifest(root).providers[0].checkin, undefined);
  assert.deepEqual(publicManifest(root).actions, []);
});

test("check-in wire rejects invalid results and preserves absent or zero grant", () => {
  assert.deepEqual(validateAccountCheckinRequest({ day: "2026-11-14", proxy: "direct" }), { day: "2026-11-14", proxy: "direct" });
  for (const value of [{ day: "2026-02-30" }, { day: "2026-11-14", proxy: "\0" }, { day: "2026-11-14", credentials: {} }])
    assert.throws(() => validateAccountCheckinRequest(value));
  for (const value of [{ outcome: "invented" }, { outcome: "claimed", credit: -1 }, { outcome: "claimed", credit: Infinity },
    { outcome: "claimed", streak: 2147483648 }, { outcome: "done", until: "tomorrow" },
    ...["2026-02-30T03:04:05Z", "2026-11-14T3:04:05Z", "2026-11-14T03:04:05+24:00",
      "2026-11-14T03:04:05,1Z", "2026-11-14T03:04:05.1234567890Z"].map(until => ({ outcome: "done", until })),
    { outcome: "done", message: "\0" }])
    assert.throws(() => validateAccountCheckinResult(value));
  assert.equal(validateAccountCheckinResult({ outcome: "claimed" }).credit, undefined);
  assert.equal(validateAccountCheckinResult({ outcome: "claimed", credit: 0 }).credit, 0);
});
