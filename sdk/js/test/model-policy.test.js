import assert from "node:assert/strict";
import { test } from "node:test";
import { SDKError, publicManifest, validateManifest } from "../index.js";
import { harness, manifest } from "./helpers.js";

function policyManifest(model) {
  return {
    ...manifest(["outbound-executor", "model-discovery"]),
    providers: [{ id: "sample", name: "Sample", protocols: ["chat"], models: [{ id: "custom-model", ...model }] }],
  };
}

test("model price preserves unknown, null and explicit free objects", () => {
  for (const price of [undefined, null, {}, { currency: "USD" }, { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, cacheWrite1h: 0 }]) {
    const result = publicManifest(validateManifest(policyManifest({ price }))).providers[0].models[0];
    if (price === undefined) assert.equal(Object.hasOwn(result, "price"), false);
    else assert.deepEqual(result.price, price);
  }
});

test("model prices accept finite nonnegative USD rates and reject malformed values", () => {
  const fields = ["input", "output", "cacheRead", "cacheWrite", "cacheWrite1h"];
  for (const field of fields) {
    for (const value of [0, 0.25, 1e20, Number.MAX_VALUE, Number.MIN_VALUE]) {
      const source = policyManifest({ price: { currency: "USD", [field]: value } });
      const frozen = publicManifest(validateManifest(source)).providers[0].models[0];
      assert.equal(frozen.price[field], value);
      assert.equal(typeof frozen.price[field], "number");
    }
    for (const value of [-0.01, NaN, Infinity, -Infinity, null, "3", true]) {
      assert.throws(() => validateManifest(policyManifest({ price: { [field]: value } })), SDKError, field + ": " + value);
    }
  }
  for (const currency of ["EUR", "usd", "USD ", "unknown", null, 3]) {
    assert.throws(() => validateManifest(policyManifest({ price: { currency } })), SDKError);
  }
  for (const price of [false, 1, "free", [], NaN, Infinity, { cache_write: 1 }]) {
    assert.throws(() => validateManifest(policyManifest({ price })), SDKError);
  }
});

test("model efforts use exactly the bounded unique canonical core levels", () => {
  const efforts = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];
  assert.deepEqual(validateManifest(policyManifest({ efforts })).providers[0].models[0].efforts, efforts);
  assert.deepEqual(validateManifest(policyManifest({ efforts: [] })).providers[0].models[0].efforts, []);
  for (const invalid of [
    ["auto"], ["light"], ["extra_high"], ["High"], [""], ["unknown"], ["ultra"], [null],
    ["low", "low"], [...efforts, "low"], "high",
  ]) assert.throws(() => validateManifest(policyManifest({ efforts: invalid })), SDKError);
});

test("model price and effort snapshots retain independent immutable nested values", () => {
  const source = policyManifest({ price: { currency: "USD", input: 3, output: 5, cacheWrite1h: 2 }, efforts: ["low", "high"] });
  const frozen = validateManifest(source), published = publicManifest(frozen);
  source.providers[0].models[0].price.input = 99;
  source.providers[0].models[0].efforts[0] = "none";
  const model = published.providers[0].models[0];
  assert.equal(model.price.input, 3);
  assert.deepEqual(model.efforts, ["low", "high"]);
  assert.throws(() => { model.price.output = 77; }, TypeError);
  assert.throws(() => model.efforts.push("ultra"), TypeError);
});

test("worker describe and model callbacks preserve unknown versus free prices on the wire", async () => {
  const models = [
    { id: "unknown" }, { id: "nullable", price: null }, { id: "free", price: {} },
    { id: "priced", price: { currency: "USD", input: 3, output: 5, cacheRead: 0.5, cacheWrite: 1, cacheWrite1h: 2 }, efforts: ["low", "high"] },
  ];
  const source = policyManifest();
  source.providers[0].models = models;
  const worker = harness({
    manifest: source,
    handlers: { "provider.execute": async () => null, "provider.models": async () => ({ models }) },
  });
  try {
    worker.send({ kind: "call", id: "describe", method: "sdk.describe" });
    assert.deepEqual((await worker.received.next()).result.providers[0].models, models);
    worker.send({ kind: "call", id: "models", method: "provider.models" });
    assert.deepEqual((await worker.received.next()).result.models, models);
  } finally { await worker.close(); }
});
