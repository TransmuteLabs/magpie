import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { createAccountWorkflow } from "../ui.js";
import { SDKError, validateAccountSelection } from "../index.js";
import { SDKError as ByteError } from "../bytes.js";
import { SDKError as BrowserError } from "../errors.js";

test("browser and Go use the same account-selection cases and SDKError identity", async () => {
  assert.equal(ByteError, SDKError);
  assert.equal(BrowserError, SDKError);
  assert.doesNotThrow(() => new SDKError("fixture", "🤖".repeat(1024)));
  assert.throws(() => new SDKError("fixture", "🤖".repeat(1025)), TypeError);
  const cases = JSON.parse(await readFile(new URL("./fixtures/account-workflow.json", import.meta.url)));
  for (const { value, valid } of cases) {
    if (valid) assert.equal(validateAccountSelection(value), value);
    else assert.throws(() => validateAccountSelection(value), SDKError);
  }
});

test("workflow enumerates before selection and invalidates changed generations without adoption", async () => {
  const previous = globalThis.window;
  let generation = "9007199254740993", calls = 0, reject = false;
  globalThis.window = { magpie: { backend: {
    accounts: async () => [{ id: "chosen", generation, name: "Reader" }],
    call: async (_binding, _input, options) => {
      calls++;
      if (reject) throw new Error("host refused");
      return options.account.expectedGeneration;
    },
  } } };
  try {
    const workflow = createAccountWorkflow("inspect");
    assert.throws(() => workflow.select("chosen"));
    assert.throws(() => workflow.call());
    await workflow.refresh();
    assert.equal(workflow.snapshot.selection, null);
    assert.equal(workflow.snapshot.accounts[0].name, "Reader");
    assert.throws(() => workflow.select("foreign"));
    workflow.select("chosen");
    assert.equal(await workflow.call(), generation);
    assert.throws(() => { workflow.snapshot.selection.id = "foreign"; });
    generation = "9007199254740994";
    await workflow.refresh();
    assert.equal(workflow.snapshot.selection, null);
    assert.equal(workflow.snapshot.stale, true);
    workflow.select("chosen");
    reject = true;
    await assert.rejects(workflow.call());
    assert.equal(calls, 2);
    assert.equal(workflow.snapshot.selection, null);
    assert.equal(workflow.snapshot.stale, true);
    assert.equal(workflow.snapshot.accounts.length, 0);
    assert.throws(() => workflow.select("chosen"));
    assert.throws(() => workflow.select("foreign"));
  } finally { globalThis.window = previous; }
});

test("workflow coalesces auth intents, never selects a completion id and cancels during start", async () => {
  const previous = globalThis.window;
  let startResolve, starts = 0, cancels = 0;
  globalThis.window = { magpie: { backend: { accounts: async () => [], call() {} }, auth: {
    start() { starts++; return new Promise(resolve => { startResolve = resolve; }); },
    finish: async () => ({ flow: "host-flow", state: "complete", account: { id: "new-host-account", generation: "1" } }),
    cancel: async () => { cancels++; return { flow: "host-flow", state: "canceled" }; },
  } } };
  try {
    const workflow = createAccountWorkflow("inspect", "signin");
    const first = workflow.start({ method: "device" });
    assert.equal(workflow.start({ method: "device" }), first);
    assert.throws(() => workflow.start({ method: "other" }));
    const canceled = workflow.cancel();
    assert.equal(workflow.cancel(), canceled);
    startResolve({ flow: "host-flow", state: "waiting", userCode: "SAFE" });
    await first;
    assert.equal((await canceled).state, "canceled");
    assert.equal(starts, 1);
    assert.equal(cancels, 1);
    assert.equal(workflow.snapshot.selection, null);
    const next = workflow.start({ method: "device" });
    startResolve({ flow: "host-flow", state: "waiting" });
    await next;
    await workflow.finish();
    assert.equal(workflow.snapshot.selection, null);
    assert.deepEqual(workflow.snapshot.accounts, []);
    assert.throws(() => workflow.select("new-host-account"));
  } finally { globalThis.window = previous; }
});

test("failed starts do not silently become a fresh exchange and older lists cannot overwrite newer ones", async () => {
  const previous = globalThis.window;
  const resolvers = [];
  let starts = 0;
  globalThis.window = { magpie: { backend: {
    accounts: () => new Promise(resolve => resolvers.push(resolve)), call() {},
  }, auth: { start: async () => { starts++; throw new Error("lost reply"); }, finish() {}, cancel() {} } } };
  try {
    const workflow = createAccountWorkflow("inspect", "signin");
    const first = workflow.start({ method: "device" });
    await assert.rejects(first);
    assert.equal(workflow.start({ method: "device" }), first);
    await assert.rejects(first);
    assert.equal(starts, 1);
    const older = workflow.refresh(), newer = workflow.refresh();
    resolvers[1]([{ id: "chosen", generation: "2" }]);
    await newer;
    resolvers[0]([{ id: "chosen", generation: "1" }]);
    await older;
    assert.equal(workflow.select("chosen").expectedGeneration, "2");
  } finally { globalThis.window = previous; }
});
