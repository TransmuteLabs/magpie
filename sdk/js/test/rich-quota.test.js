import assert from "node:assert/strict";
import { test } from "node:test";
import { validateUsageResult } from "../index.js";

const manifest = minor => ({ contract: { version: { major: 1, minor } } });
const result = fields => ({ windows: [{ name: "quota", used: 0, ...fields }] });

test("rich quota requires API9 even for explicit empty metadata", () => {
  for (const [field, value] of Object.entries({ amount: 0, balance: 0, display: "", unit: "", span: 0 })) {
    assert.throws(() => validateUsageResult(manifest(8), result({ [field]: value })));
  }
});

test("rich quota validates native duration, text and exact numeric bounds", () => {
  for (const fields of [
    { amount: -1 }, { amount: 9007199254740992 }, { amount: "0" }, { amount: NaN },
    { balance: -9007199254740992 }, { balance: true }, { balance: Infinity },
    { limit: -1 }, { limit: 9007199254740992 }, { limit: "0" },
    { display: null }, { display: "line\nbreak" }, { display: "a".repeat(513) },
    { unit: null }, { unit: "界".repeat(22) }, { span: null }, { span: -1 }, { span: 0.5 }, { span: 9223372037 },
  ]) assert.throws(() => validateUsageResult(manifest(9), result(fields)), JSON.stringify(fields));
  for (const fields of [
    { amount: 0 }, { amount: null }, { balance: 0 }, { balance: -1.25 }, { balance: null },
    { span: 9223372036 }, { display: "vendor units", unit: "ACU" },
  ]) assert.equal(validateUsageResult(manifest(9), result(fields)), true);
});

test("quota metadata supplies no missing share, historical authority or execution cost", () => {
  for (const window of [
    { name: "quota", amount: 0, limit: 10 }, { name: "quota", balance: 20 },
    { name: "quota", used: 0, unknown: true, amount: 0 },
    { name: "quota", used: 0, asOf: 1, amount: 0 }, { name: "quota", used: 0, aside: true, amount: 0 },
  ]) assert.equal(validateUsageResult(manifest(9), { windows: [window] }), false);
});
