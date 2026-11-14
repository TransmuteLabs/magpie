import assert from "node:assert/strict";
import test from "node:test";
import { accountKeepAliveLead } from "../index.js";

test("authentication keepAlive default is one minute and absence stays off", () => {
  assert.equal(accountKeepAliveLead(), 0);
  assert.equal(accountKeepAliveLead(null), 0);
  assert.equal(accountKeepAliveLead({}), 60000);
  assert.equal(accountKeepAliveLead({ leadMs: undefined }), 60000);
  assert.equal(accountKeepAliveLead({ leadMs: 3600000 }), 3600000);
  assert.throws(() => accountKeepAliveLead({ leadMs: null }));
});
