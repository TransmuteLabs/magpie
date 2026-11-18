const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");
const vm = require("node:vm");

const source = fs.readFileSync(path.resolve(__dirname, "../assets/app.js"), "utf8");
const predicate = source.match(/^const kimiSub = .+;$/m)?.[0];
const identity = source.match(/^const kindOf = .+;$/m)?.[0];
assert.ok(predicate && identity, "ordinary renderer predicates are present");
const accepts = vm.runInNewContext(identity + "\n" + predicate + "\nkimiSub");
const fields = ["model", "pool", "effort"].map((name) => ({
  key: "subagent_" + name, label: "subagent " + name,
}));
const native = {
  provider: "connected", runtime: "available",
  fields: Object.fromEntries(fields.map((field) => [
    field.key, { value: "magpie/fixture/alpha", status: "connected" },
  ])),
};

for (const [name, agent, expected] of [
  ["built-in Kimi full shape", { id: "kimi", fields }, true],
  ["built-in Kimi WSL identity", { id: "kimi@wsl:Ubuntu", fields }, true],
  ["ordinary non-Kimi full shape", { id: "worker-pool", fields }, false],
  ["SDK normal native metadata", { id: "worker-pool", fields, native }, false],
  ["native owner excludes specialized config renderer", { id: "kimi", fields, native }, false],
  ["legacy Kimi without pool", { id: "kimi", fields: [] }, false],
  ["incomplete shape", { id: "kimi", fields: fields.slice(0, 2) }, false],
  ["different label contract", { id: "kimi", fields: fields.map((field) => ({ ...field, label: "Worker field" })) }, false],
]) {
  test(name, () => assert.equal(accepts(agent), expected));
}
