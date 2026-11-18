import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import { test } from "node:test";
import { ui } from "../ui.js";

test("selected-account parent bridge fixes ref and intent while keeping scope out of business input", async () => {
  const source = await readFile(new URL("../../../internal/gui/assets/extensionwindows.js", import.meta.url), "utf8");
  const requests = [];
  const summary = [{ id: "account-a", generation: "9007199254740993" }];
  const parent = {
    crypto: { getRandomValues(bytes) { bytes.fill(5); return bytes; } },
    t: message => message,
    api: async (path, body) => { requests.push({ path, body }); return path.endsWith("accounts") ? summary : { done: true }; },
  };
  vm.runInNewContext(source.replace(/^export /gm, ""), parent);
  const frame = { entry: { ref: "HOST-ISSUED-REF" } };
  const previous = globalThis.window;
  globalThis.window = { magpie: { backend: {
    accounts(binding) { return parent.answerPluginPage(frame, { id: "list", method: "backend.accounts", params: { binding } }); },
    call(binding, input, options) {
      return parent.answerPluginPage(frame, { id: "write-one", method: "backend.call",
        params: { binding, input, options, ref: "FORGED", scope: { provider: "foreign", account: "foreign" } } });
    },
  }, config: { get() {}, cas() {} } } };
  try {
    assert.deepEqual(await ui.backend.accounts("page.reset"), summary);
    const account = { id: "account-a", expectedGeneration: summary[0].generation };
    const input = { account: "business-data", auth: "not-credentials" };
    assert.deepEqual(await ui.backend.call("page.reset", input, { account }), { done: true });
    await ui.backend.call("page.reset", input, { account });
    assert.equal(requests[0].path, "plugin-pages/accounts");
    assert.equal(requests[0].body.ref, "HOST-ISSUED-REF");
    assert.equal(requests[1].body.ref, "HOST-ISSUED-REF");
    assert.equal(requests[1].body.account, account);
    assert.equal(requests[1].body.input, input);
    assert.equal(requests[1].body.scope, undefined);
    assert.equal(requests[1].body.request, requests[2].body.request);
    await assert.rejects(parent.answerPluginPage({ entry: {} }, { method: "backend.accounts" }), /No such call/);
  } finally {
    globalThis.window = previous;
  }
});

test("typed selected-account facade delegates through the actual isolated page bridge", async () => {
  const source = await readFile(new URL("../../../internal/gui/pagebridge.js", import.meta.url), "utf8");
  const messages = [], listeners = new Map(), parent = { postMessage(message) { messages.push(message); } };
  const page = {
    window: {}, parent, URL, setTimeout, clearTimeout, Blob,
    addEventListener(name, listener) { listeners.set(name, [...(listeners.get(name) ?? []), listener]); },
    document: { readyState: "loading", documentElement: { dataset: {}, style: { setProperty() {} } },
      createElement() { return { dataset: {} }; }, head: { prepend() {} } },
    location: { href: "https://magpie.invalid/_magpie/page/account/index.html" },
  };
  vm.runInNewContext(source, page);
  const deliver = data => { for (const listener of listeners.get("message") ?? []) listener({ source: parent, data }); };
  const previous = globalThis.window;
  globalThis.window = page.window;
  try {
    const hello = messages.shift();
    deliver({ magpie: 1, id: hello.id, result: { dark: false, lang: "en", tokens: {} } });
    await ui.ready;
    const list = ui.backend.accounts("page.reset");
    const listed = messages.shift();
    assert.equal(listed.method, "backend.accounts");
    deliver({ magpie: 1, id: listed.id, result: [{ id: "one", generation: "18446744073709551615" }] });
    assert.equal((await list)[0].generation, "18446744073709551615");
    const account = { id: "one", expectedGeneration: "18446744073709551615" };
    const pending = ui.backend.call("page.reset", { window: "week" }, { account });
    const sent = messages.shift();
    assert.equal(sent.params.options.account.expectedGeneration, account.expectedGeneration);
    assert.equal(sent.params.input.window, "week");
    deliver({ magpie: 1, id: sent.id, result: { done: true } });
    assert.deepEqual(await pending, { done: true });
  } finally {
    page.window.__magpieDispose?.();
    globalThis.window = previous;
  }
});
