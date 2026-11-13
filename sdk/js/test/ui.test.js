import assert from "node:assert/strict";
import { test } from "node:test";
import { readFile } from "node:fs/promises";
import { ui } from "../ui.js";

test("browser entry has no Node/Bun runtime imports and delegates the exact existing bridge", async () => {
  const source = await readFile(new URL("../ui.js", import.meta.url), "utf8");
  assert(!/\bimport\b|\brequire\s*\(|\bBuffer\b|\bBun\b|\bprocess\b/.test(source));
  const previous = globalThis.window;
  const calls = [];
  const snapshot = { version: "2", values: { mode: "current" } };
  const ready = Promise.resolve({ dark: true });
  globalThis.window = { magpie: {
    ready, service: "https://magpie.invalid/_magpie/plugin/revision/service/",
    backend: { call(binding, input) { calls.push(["call", binding, input]); return Promise.resolve({ ok: true }); } },
    config: {
      get() { calls.push(["get"]); return Promise.resolve(snapshot); },
      cas(expected, values) { calls.push(["cas", expected, values]); return Promise.resolve({ version: "3", values }); },
    },
  } };
  try {
    assert.equal(ui.ready, ready);
    assert.equal((await ui.ready).dark, true);
    assert.equal(ui.service, globalThis.window.magpie.service);
    assert.deepEqual(await ui.backend.call("page.start", { prompt: "hello" }), { ok: true });
    assert.equal(await ui.config.get(), snapshot);
    assert.deepEqual(await ui.config.cas("2", { mode: "next" }), { version: "3", values: { mode: "next" } });
    assert.deepEqual(calls, [["call", "page.start", { prompt: "hello" }], ["get"], ["cas", "2", { mode: "next" }]]);
    const disposed = new Error("Frame disposed");
    globalThis.window.magpie.backend.call = () => Promise.reject(disposed);
    await assert.rejects(ui.backend.call("page.start"), error => error === disposed);
    globalThis.window = undefined;
    assert.throws(() => ui.config.get(), /bridge is unavailable/);
  } finally {
    if (previous === undefined) delete globalThis.window;
    else globalThis.window = previous;
  }
});

test("published package contains server dependencies and a separate browser ui export", async () => {
  const pkg = JSON.parse(await readFile(new URL("../package.json", import.meta.url), "utf8"));
  assert.deepEqual(pkg.exports["./ui"], { types: "./ui.d.ts", default: "./ui.js" });
  for (const file of ["composite.js", "lifecycle.js", "ui.js", "ui.d.ts"]) {
    assert(pkg.files.includes(file));
    assert((await readFile(new URL("../" + file, import.meta.url))).length);
  }
});

test("browser facade exposes existing theme, context, visibility, actions, reads and image operations", async () => {
  const previous = globalThis.window;
  const calls = [];
  const theme = { dark: true, lang: "en", tokens: { bg: "#101010" } };
  const context = { provider: { id: "fixture", name: "Fixture" } };
  let themeListener, visibilityListener, actionListener;
  const png = new Blob(["fixture"], { type: "image/png" });
  globalThis.window = { magpie: {
    ready: Promise.resolve(theme), theme, context, visible: true,
    backend: { call() {} }, config: { get() {}, cas() {} },
    onTheme(listener) { themeListener = listener; },
    onVisibility(listener) { visibilityListener = listener; },
    onAction(listener) { actionListener = listener; },
    read(name, params) { calls.push(["read", name, params]); return Promise.resolve({ items: [] }); },
    image: {
      copy(value) { calls.push(["copy", value]); return Promise.resolve({ ok: true }); },
      save(value, name) { calls.push(["save", value, name]); return Promise.resolve({ path: "" }); },
    },
  } };
  try {
    assert.equal(ui.theme, theme);
    assert.equal(ui.context, context);
    assert.equal(ui.visible, true);
    ui.onTheme(value => calls.push(["theme", value]));
    ui.onVisibility(value => calls.push(["visible", value]));
    ui.onAction(value => calls.push(["action", value]));
    const nextTheme = { ...theme, dark: false };
    themeListener(nextTheme);
    visibilityListener(false);
    actionListener({ id: "fixture.action", provider: context.provider });
    globalThis.window.magpie.theme = nextTheme;
    globalThis.window.magpie.visible = false;
    assert.equal(ui.theme, nextTheme);
    assert.equal(ui.visible, false);
    assert.deepEqual(await ui.read("providers:list", { fixture: true }), { items: [] });
    assert.deepEqual(await ui.image.copy(png), { ok: true });
    assert.deepEqual(await ui.image.save(png, "fixture"), { path: "" });
    assert.deepEqual(calls, [
      ["theme", nextTheme], ["visible", false], ["action", { id: "fixture.action", provider: context.provider }],
      ["read", "providers:list", { fixture: true }], ["copy", png], ["save", png, "fixture"],
    ]);
    const denied = new Error("Read is not declared");
    globalThis.window.magpie.read = () => Promise.reject(denied);
    await assert.rejects(ui.read("not-declared"), error => error === denied);
  } finally {
    if (previous === undefined) delete globalThis.window;
    else globalThis.window = previous;
  }
});
