import assert from "node:assert/strict";
import { test } from "node:test";
import { validateExecutionUsage } from "../index.js";

const manifest = { contract: { version: { major: 1, minor: 7 } } };

test("sparse normalized usage preserves zero and absent raw facts", () => {
  const usage = { facts: {
    inputTokens: 0, cacheWriteTokens: 7, cacheWrite1hTokens: 3, reasoningTokens: 0,
    cacheUnknown: false, tier: "", deferred: { inputTokens: 12 }, credits: 0,
    cost: { amount: 0, currency: "USD" },
  } };
  assert.doesNotThrow(() => validateExecutionUsage(manifest, usage));
  const copy = JSON.parse(JSON.stringify(usage));
  assert.equal(Object.hasOwn(copy.facts, "outputTokens"), false);
  assert.equal(copy.facts.inputTokens, 0);
  assert.equal(copy.facts.cacheUnknown, false);
  assert.equal(copy.facts.tier, "");
  assert.equal(copy.facts.credits, 0);
  assert.equal(copy.facts.cost.amount, 0);
  for (const legacy of [undefined, null, {}, { inputTokens: 12, outputTokens: 3, cachedTokens: 2, cacheWriteTokens: 1, cost: 0.5, currency: "USD" }]) {
    assert.doesNotThrow(() => validateExecutionUsage({ contract: { version: { minor: 6 } } }, legacy));
  }
});

test("execution usage rejects mixed counters invalid facts and unnegotiated versions", () => {
  for (const usage of [
    { facts: { inputTokens: -1 } }, { facts: { inputTokens: 2 ** 53 } },
    { facts: { cacheWriteTokens: 2, cacheWrite1hTokens: 3 } },
    { facts: { deferred: { outputTokens: -1 } } },
    { inputTokens: 1, facts: {} }, { cost: 1, facts: {} }, { currency: "USD", facts: {} },
    { facts: { credits: Infinity } }, { facts: { credits: -1 } },
    { facts: { cost: { amount: 1 } } }, { facts: { cost: { amount: -1, currency: "USD" } } },
    { facts: { cost: { amount: NaN, currency: "USD" } } },
    { facts: { cost: { amount: 0, currency: "x".repeat(17) } } },
    { facts: { servedModel: "model\nforged" } }, { facts: { requestId: "x".repeat(301) } },
    { cost: Infinity }, { facts: { cacheUnknown: 1 } }, { facts: [] },
  ]) assert.throws(() => validateExecutionUsage(manifest, usage));
  assert.throws(() => validateExecutionUsage({ contract: { version: { minor: 6 } } }, { facts: {} }));
});
