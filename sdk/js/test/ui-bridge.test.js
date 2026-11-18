import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";
import { test } from "node:test";
import { ui } from "../ui.js";

test("typed facade uses the real isolated page bridge for events, permissions, images and disposal", async () => {
  const source = await readFile(new URL("../../../internal/gui/pagebridge.js", import.meta.url), "utf8");
  const messages = [];
  const listeners = new Map();
  const parent = { postMessage(message) { messages.push(message); } };
  const page = { window: {}, parent, URL, setTimeout, clearTimeout, Blob,
    addEventListener(name, listener) {
      const values = listeners.get(name) ?? [];
      values.push(listener);
      listeners.set(name, values);
    },
    document: {
      readyState: "loading",
      documentElement: { dataset: {}, style: { setProperty() {} } },
      createElement() { return { dataset: {} }; },
      head: { prepend() {} },
    },
    location: { href: "https://magpie.invalid/_magpie/page/fixture/index.html" },
  };
  vm.runInNewContext(source, page, { filename: "pagebridge.js" });
  const deliver = (data, sender = parent) => {
    for (const listener of listeners.get("message") ?? []) listener({ source: sender, data });
  };
  const previous = globalThis.window;
  globalThis.window = page.window;
  try {
    const hello = messages.shift();
    assert.equal(hello.method, "hello");
    let settled = false;
    ui.ready.then(() => { settled = true; });
    deliver({ magpie: 1, id: hello.id, result: { dark: false } }, {});
    await Promise.resolve();
    assert.equal(settled, false);
    deliver({ magpie: 1, id: hello.id, result: {
      dark: true, lang: "de", tokens: { bg: "#000000" },
      context: { provider: { id: "fixture", name: "Fixture" } },
      service: "../service/", visible: false,
    } });
    assert.equal((await ui.ready).lang, "de");
    assert.equal(ui.context.provider.id, "fixture");
    assert.equal(ui.visible, false);
    assert.equal(ui.service, "https://magpie.invalid/_magpie/page/service/");
    const events = [];
    ui.onTheme(theme => events.push(["theme", theme.lang]));
    ui.onVisibility(visible => events.push(["visible", visible]));
    deliver({ magpie: 1, event: "action", data: { id: "fixture.open", provider: { id: "fixture", name: "Fixture" } } });
    ui.onAction(action => events.push(["action", action.id]));
    deliver({ magpie: 1, event: "theme", data: { dark: false, lang: "en", tokens: {} } });
    deliver({ magpie: 1, event: "visibility", data: { visible: true } });
    assert.equal(ui.theme.lang, "en");
    assert.equal(ui.visible, true);
    assert.deepEqual(events, [["action", "fixture.open"], ["theme", "en"], ["visible", true]]);

    const reading = ui.read("providers:list", { fixture: true });
    const request = messages.shift();
    assert.equal(request.method, "read");
    assert.equal(request.params.name, "providers:list");
    assert.equal(request.params.params.fixture, true);
    deliver({ magpie: 1, id: request.id, error: "Read is not declared" });
    await assert.rejects(reading, /Read is not declared/);

    const png = new Blob(["fixture"], { type: "image/png" });
    const copying = ui.image.copy(png);
    const copy = messages.shift();
    assert.equal(copy.method, "image.copy");
    assert.equal(copy.params.png, png);
    deliver({ magpie: 1, id: copy.id, result: { ok: true } });
    assert.equal((await copying).ok, true);
    const saving = ui.image.save(png, "fixture");
    const save = messages.shift();
    assert.equal(save.method, "image.save");
    assert.equal(save.params.name, "fixture");
    deliver({ magpie: 1, id: save.id, result: { path: "" } });
    assert.equal((await saving).path, "");
    deliver({ magpie: 1, event: "dispose" });
    await assert.rejects(ui.read("providers:list"), /disposed/);
    assert.equal(messages.length, 0);
  } finally {
    if (previous === undefined) delete globalThis.window;
    else globalThis.window = previous;
  }
});
