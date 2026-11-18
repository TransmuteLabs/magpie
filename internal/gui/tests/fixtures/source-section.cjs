const assert = require("node:assert/strict");

function sourceSection(source, start, end) {
  const first = source.indexOf(start), last = source.indexOf(end);
  assert(first >= 0, `missing section start: ${start}`);
  assert(last >= 0, `missing section end: ${end}`);
  assert.equal(source.indexOf(start, first + start.length), -1, `ambiguous section start: ${start}`);
  assert.equal(source.indexOf(end, last + end.length), -1, `ambiguous section end: ${end}`);
  assert(last > first, `section end precedes start: ${start}`);
  return source.slice(first, last);
}

module.exports = { sourceSection };
