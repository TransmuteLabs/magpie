const assert = require("node:assert/strict");
const { test } = require("node:test");
const { sourceSection } = require("./fixtures/source-section.cjs");

test("source sections keep their named declaration and stop at the next one", () => {
  assert.equal(sourceSection("preamble\nlet first = 1;\nfunction next() {}\n", "let first =", "function next("),
    "let first = 1;\n");
});

test("source sections reject missing, ambiguous and reversed declarations", () => {
  for (const [source, message] of [
    ["function next() {}", /missing section start/],
    ["let first = 1;", /missing section end/],
    ["let first = 1; let first = 2; function next() {}", /ambiguous section start/],
    ["let first = 1; function next() {} function next() {}", /ambiguous section end/],
    ["function next() {} let first = 1;", /precedes start/],
  ]) {
    assert.throws(() => sourceSection(source, "let first =", "function next("), message);
  }
});
