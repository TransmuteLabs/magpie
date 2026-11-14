const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(path.join(__dirname, "../assets/app.js"), "utf8");
const start = source.indexOf("let quotaLeft = false;");
const end = source.indexOf("// familyWindows:", start);
assert.ok(start >= 0 && end > start);
const presentation = vm.runInNewContext(source.slice(start, end) + `
  ({ text: quotaText, count: quotaCount, pace: quotaPace,
     left: value => { quotaLeft = value; } });
`, {
  t: (value, fields) => Object.entries(fields || {}).reduce((text, [key, replacement]) =>
    text.replaceAll("{" + key + "}", replacement), value),
});

test("ordinary quota formatter distinguishes missing amount and exact zero", () => {
  for (const left of [false, true]) {
    presentation.left(left);
    assert.equal(presentation.count({ used: 25, limit: 10, unit: "ACU" }), "",
      "missing vendor amount must not become zero");
    assert.equal(presentation.count({ used: 0, amount: 0, limit: 10, unit: "ACU" }),
      `${left ? 10 : 0} / 10 ACU`);
    assert.equal(presentation.count({ used: 25, amount: 2.5, limit: 10, unit: "ACU" }),
      `${left ? 7.5 : 2.5} / 10 ACU`);
    assert.equal(presentation.text({ used: 0, balance: 0, text: true, unit: "ACU" }), "0 ACU left");
    assert.equal(presentation.text({ used: 0, text: true, unknown: true, display: "Gift pending" }), "Gift pending");
  }
});

test("unknown and wallet metadata cannot invent pace or measured monetary cost", () => {
  const now = Date.now(), startsAt = new Date(now - 3600e3).toISOString(), resetsAt = new Date(now + 3600e3).toISOString();
  presentation.left(false);
  assert.equal(presentation.pace({ used: 25, startsAt, resetsAt }, now).n, -25);
  for (const fields of [{ text: true }, { balance: 0 }, { historyMissing: true }]) {
    assert.equal(presentation.pace({ used: 25, startsAt, resetsAt, ...fields }, now), null);
  }
  assert.equal(presentation.text({ text: true, used: 0, display: "vendor quota" }), "vendor quota");
  assert.ok(!presentation.text({ used: 25, amount: 2.5, limit: 10, unit: "ACU" }).includes("$"));
});
