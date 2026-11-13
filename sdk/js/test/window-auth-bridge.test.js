import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import { test } from "node:test";
import { ui } from "../ui.js";

test("authentication parent fixes frame authority and exposes only declared operations", async () => {
  const source = await readFile(new URL("../../../internal/gui/assets/extensionwindows.js", import.meta.url), "utf8");
  const requests = [];
  const parent = {
    crypto: { getRandomValues(bytes) { bytes.fill(9); return bytes; } },
    t: message => message,
    api: async (path, body) => { requests.push({ path, body }); return { flow: "HOST-FLOW", state: "waiting" }; },
  };
  vm.runInNewContext(source.replace(/^export /gm, ""), parent);
  const frame = { entry: { ref: "HOST-REF" } };
  for (const method of ["auth.start", "auth.finish", "auth.cancel"]) {
    await parent.answerPluginPage(frame, { id: method, method, params: {
      binding: "signin", flow: "HOST-FLOW", options: { method: "device", input: { project: "one" } },
      parent: "FORGED", ref: "FORGED", request: "FORGED", provider: "foreign", scope: { auth: "FORGED" },
    } });
  }
  for (const { path, body } of requests) {
    assert.equal(path, "plugin-pages/auth");
    assert.equal(body.ref, "HOST-REF");
    assert.equal(body.parent, frame.instance);
    assert(body.request.startsWith(frame.instance + ":"));
    assert.equal(body.scope, undefined);
    assert.equal(body.provider, undefined);
  }
  assert.equal(requests[0].body.operation, "start");
  assert.equal(requests[0].body.flow, undefined);
  assert.equal(requests[1].body.options, undefined);
  assert.equal(requests[2].body.operation, "cancel");
  await assert.rejects(parent.answerPluginPage({ entry: {} }, { method: "auth.start" }), /No such call/);
});

test("authentication facade reaches the actual isolated bridge with no raw credential result", async () => {
  const source = await readFile(new URL("../../../internal/gui/pagebridge.js", import.meta.url), "utf8");
  const messages = [], listeners = new Map(), parent = { postMessage(message) { messages.push(message); } };
  const page = {
    window: {}, parent, URL, setTimeout, clearTimeout, Blob,
    addEventListener(name, listener) { listeners.set(name, [...(listeners.get(name) ?? []), listener]); },
    document: { readyState: "loading", documentElement: { dataset: {}, style: { setProperty() {} } },
      createElement() { return { dataset: {} }; }, head: { prepend() {} } },
    location: { href: "https://magpie.invalid/_magpie/page/auth/index.html" },
  };
  vm.runInNewContext(source, page);
  const deliver = data => { for (const listener of listeners.get("message") ?? []) listener({ source: parent, data }); };
  const previous = globalThis.window;
  globalThis.window = page.window;
  try {
    const hello = messages.shift();
    deliver({ magpie: 1, id: hello.id, result: { dark: false, lang: "en", tokens: {} } });
    await ui.ready;
    const pending = ui.auth.start("signin", { method: "device" });
    const start = messages.shift();
    assert.equal(start.method, "auth.start");
    assert.equal(start.params.options.method, "device");
    deliver({ magpie: 1, id: start.id, result: { flow: "host-flow", state: "waiting", userCode: "SAFE" } });
    assert.equal((await pending).userCode, "SAFE");
    const finished = ui.auth.finish("signin", "host-flow");
    const finish = messages.shift();
    assert.equal(finish.method, "auth.finish");
    assert.equal(finish.params.flow, "host-flow");
    deliver({ magpie: 1, id: finish.id, result: { flow: "host-flow", state: "complete", account: { id: "chosen", generation: "1" } } });
    assert.equal((await finished).account.generation, "1");
  } finally {
    page.window.__magpieDispose?.();
    globalThis.window = previous;
  }
});
