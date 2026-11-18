// SDK windows use the existing renderer and bridge. All files and APIs below
// are intercepted fixtures; no app, live configuration or network is used.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(process.env.MAGPIE_ASSETS || path.join(__dirname, "../assets"));
const bridge = path.resolve(__dirname, "../pagebridge.js");
const tokens = ["a", "b", "c", "d", "e", "f"].map((c) => c.repeat(32));
const HTML = `<!doctype html><meta charset="utf-8"><script src="_magpie/bridge.js"></script><p id="ready">SDK window</p>`;

async function fixture(page, lang, web, slot = "agents", settingsSibling = false) {
  const state = { generation: 0, enabled: true, unknown: false, calls: [], configs: [], options: [], holds: [], canceled: [], version: 0, notices: [] };
  state.history = []; state.delays = new Map();
  const entry = (panel) => {
    const tok = tokens[state.generation * 2 + (panel ? 1 : 0)];
    return { spec: "sdk:composite", name: "composite", key: panel ? "extension-composite-0" : "extension-composite",
      title: "Composite", titles: settingsSibling ? { zh: "组合面板" } : undefined,
      ref: tok, reads: ["providers:list"], slot: panel ? slot : undefined,
      url: `/_magpie/plugin/${tok}/index.html`, service: "_magpie/service/page.service/" };
  };
  const sibling = {
    ...entry(true), spec: "sdk:sibling-package", name: "sibling-package", title: "Other controls",
    titles: undefined, key: "extension-sibling-0", ref: tokens[5],
    url: `/_magpie/plugin/${tokens[5]}/index.html`,
  };
  state.notify = () => {
    state.version++;
    for (const done of state.notices.splice(0)) done();
  };
  page.on("requestfailed", (req) => { if (req.url().includes("/api/plugin-pages/call")) state.canceled.push(req.postDataJSON()); });
  await page.route("**/*", async (route) => {
    const req = route.request(), u = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (u.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:${web}};` });
    if (u.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (u.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (u.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true }, plugins: [] });
    if (u.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (u.pathname === "/api/plugin-pages" || u.pathname === "/api/plugin-panels") {
      if (state.unknown) return route.fulfill({ status: 503, json: { error: "catalog unavailable" } });
      const panel = u.pathname.endsWith("panels");
      return json(state.enabled ? [entry(panel), ...(panel && settingsSibling ? [sibling] : [])] : []);
    }
    if (u.pathname === "/api/plugin-pages/changes") {
      if (u.searchParams.get("version") === String(state.version)) await new Promise((resolve) => state.notices.push(resolve));
      return json({ version: String(state.version) }).catch(() => {});
    }
    if (u.pathname === "/api/plugin-pages/history") {
      const body = req.postDataJSON(); state.history.push(body);
      if (state.delays.has(body.operation)) await new Promise(resolve => state.delays.get(body.operation).push(resolve));
      if (body.operation === "hello") return json({ features: ["session-history-v1"] });
      if (body.operation === "media") return route.fulfill({ contentType: "image/png", body: Buffer.from([1, 2, 3]) });
      return json({ entries: [{sessionRef:"session",title:"private history"}], sessionRef:"session", readRef:"read", parts:[], media:[] });
    }
    if (u.pathname === "/api/plugin-pages/call") {
      const body = req.postDataJSON();
      state.calls.push(body);
      if (body.binding === "hold") await new Promise((resolve) => state.holds.push(resolve));
      if (body.binding === "forbidden") return route.fulfill({ status: 403, json: { error: "binding not declared" } });
      return json({ binding: body.binding, input: body.input }).catch(() => {});
    }
    if (u.pathname === "/api/plugin-pages/config") {
      const body = req.postDataJSON();
      state.configs.push(body);
      if ("values" in body) {
        if (body.expected !== String(state.generation)) return route.fulfill({ status: 409, json: { error: "configuration conflict" } });
        return json({ version: String(state.generation + 1), values: body.values });
      }
      return json({ version: String(state.generation), values: { enabled: true } });
    }
    if (u.pathname === "/api/plugin-pages/options") { state.options.push(req.postDataJSON()); return json({}); }
    if (u.pathname === "/api/plugin-pages/read") return json({ providers: [] });
    if (u.pathname.startsWith("/api/")) return json({});
    if (u.pathname.startsWith("/_magpie/plugin/")) {
      const [tok, ...parts] = u.pathname.slice("/_magpie/plugin/".length).split("/");
      if (!state.enabled || ![entry(false).ref, entry(true).ref, ...(settingsSibling ? [sibling.ref] : [])].includes(tok)) return route.fulfill({ status: 404, body: "" });
      const name = parts.join("/");
      if (name === "index.html") return route.fulfill({ contentType: "text/html", body: HTML });
      if (name === "_magpie/bridge.js") return route.fulfill({ contentType: "text/javascript", body: await fs.readFile(bridge) });
      return route.fulfill({ status: 404, body: "" });
    }
    const file = path.join(assets, u.pathname === "/" ? "index.html" : u.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { return await route.fulfill({ body: await fs.readFile(file), contentType }); }
    catch { return route.fulfill({ status: 404, body: "" }); }
  });
  await page.goto("http://magpie.test/");
  await page.locator('#nav button[data-view="extension-composite"]').waitFor({ state: "attached" });
  await page.evaluate(() => show("extension-composite"));
  return state;
}

async function ready(page, tok) {
  await page.waitForFunction((tok) => [...document.querySelectorAll("iframe")].some((f) => f.src.includes(tok)), tok);
  const f = page.frames().find((f) => f.url().includes(tok))
    || await page.waitForEvent("framenavigated", { predicate: (f) => f.url().includes(tok) });
  assert.ok(f, "the frame is attached");
  await f.waitForFunction(() => !!window.magpie?.theme);
  return f;
}

async function mounted(t, engine, lang, slot = "agents", settingsSibling = false) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
  t.after(() => browser.close());
  const context = await browser.newContext({ viewport: { width: 1100, height: 700 }, reducedMotion: "reduce" });
  const page = await context.newPage(); page.setDefaultTimeout(8000);
  page.setDefaultNavigationTimeout(60000);
  const state = await fixture(page, lang, true, slot, settingsSibling);
  t.after(async () => {
    for (const resolve of [...state.holds, ...state.notices, ...[...state.delays.values()].flat()]) resolve();
    await context.close();
  });
  return { page, state };
}
// Hidden iframes may suspend animation frames; observe their state with timer polling.
const selection = { source: "history", viewRef: "view", rootRef: "root" };

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    test(`${engine} ${lang}: pages and panels share navigation visibility`, async t => {
      const { page, state } = await mounted(t, engine, lang);
      const panel = await ready(page, tokens[1]);
      await panel.evaluate(() => magpie.history.ready);
      assert.equal(await panel.evaluate(() => magpie.visible), false, "an initially hidden panel must report hidden");
      assert.match(await panel.evaluate(s => magpie.history.list(s).then(() => "allowed", e => e.message), selection), /unavailable/);
      assert(!state.history.some(call => call.operation === "list"));
      await page.evaluate(() => show("agents"));
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
      assert.equal((await panel.evaluate(s => magpie.history.list(s), selection)).entries.length, 1);
      await page.evaluate(() => show("extension-composite"));
      await panel.waitForFunction(() => magpie.visible === false, null, { polling: 25 });
      const frame = await ready(page, tokens[0]);
      assert.equal(await frame.evaluate(() => magpie.visible), true);
      await page.evaluate(() => show("agents"));
      await frame.waitForFunction(() => magpie.visible === false, null, { polling: 25 });
      assert.equal(await panel.evaluate(() => magpie.visible), true);
    });

    test(`${engine} ${lang}: settings subtabs and hidden ancestors control panel visibility`, async t => {
      const { page } = await mounted(t, engine, lang, "settings");
      const panel = await ready(page, tokens[1]);
      await page.evaluate(async () => { await show("settings"); await settingsLoad; setSetTab("plugins"); });
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
      await page.evaluate(() => setSetTab("general"));
      await panel.waitForFunction(() => magpie.visible === false, null, { polling: 25 });
      await page.evaluate(() => setSetTab("plugins"));
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
      await page.evaluate(() => { document.querySelector('#setPage-plugins').style.display = "none"; });
      await panel.waitForFunction(() => magpie.visible === false, null, { polling: 25 });
      await page.evaluate(() => { document.querySelector('#setPage-plugins').style.display = ""; });
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
    });

    test(`${engine} ${lang}: settings search finds SDK panels independently`, async t => {
      const { page } = await mounted(t, engine, lang, "settings", true);
      const first = page.locator('[data-panel="extension-composite-0"]');
      const second = page.locator('[data-panel="extension-sibling-0"]');
      const panel = await ready(page, tokens[1]);
      const sibling = await ready(page, tokens[5]);
      await page.evaluate(async () => { await show("settings"); await settingsLoad; setSetTab("plugins", true); });
      await first.waitFor();
      await second.waitFor();
      const search = page.locator("#setQ");
      await search.fill("Composite");
      assert.equal(await first.isVisible(), true, "the matching SDK panel remains visible");
      await second.waitFor({ state: "hidden" });
      await panel.waitForFunction(() => magpie.visible, null, { polling: 25 });
      await sibling.waitForFunction(() => !magpie.visible, null, { polling: 25 });
      if (lang.startsWith("zh")) {
        await search.fill("组合面板");
        assert.equal(await first.isVisible(), true, "the translated title matches");
        assert.equal(await second.isVisible(), false);
      }
      await search.fill("sibling-package");
      await second.waitFor();
      await first.waitFor({ state: "hidden" });
      await search.fill("missing-panel-726");
      await page.locator("#setNone").waitFor();
      await search.press("Escape");
      await first.waitFor();
      await second.waitFor();
      await page.locator("#setNone").waitFor({ state: "hidden" });
      await panel.waitForFunction(() => magpie.visible, null, { polling: 25 });
      await sibling.waitForFunction(() => magpie.visible, null, { polling: 25 });
    });

    test(`${engine} ${lang}: provider editor slots hide, reopen and dispose with their container`, async t => {
      const { page, state } = await mounted(t, engine, lang, "provider");
      await page.evaluate(() => {
        const editor = document.createElement("div"); editor.id = "fixture-provider-editor";
        editor.append(magpie.slot("provider", { provider: { id: "fixture", name: "Fixture" } }));
        document.querySelector("#view-providers").append(editor);
      });
      const panel = await ready(page, tokens[1]);
      assert.equal(await panel.evaluate(() => magpie.visible), false);
      await page.evaluate(() => show("providers"));
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
      await page.evaluate(() => { document.getElementById("fixture-provider-editor").hidden = true; });
      await panel.waitForFunction(() => magpie.visible === false, null, { polling: 25 });
      await page.evaluate(() => { document.getElementById("fixture-provider-editor").hidden = false; });
      await panel.waitForFunction(() => magpie.visible === true, null, { polling: 25 });
      const pending = page.waitForRequest(req => req.url().endsWith("/api/plugin-pages/call") && req.postDataJSON()?.binding === "hold");
      await panel.evaluate(() => { window.held = magpie.backend.call("hold", {}).catch(() => null); });
      await pending;
      const canceled = page.waitForEvent("requestfailed", { predicate: req => req.url().endsWith("/api/plugin-pages/call") });
      await page.evaluate(() => document.getElementById("fixture-provider-editor").remove());
      await canceled;
      assert(state.canceled.some(call => call.binding === "hold"), "removing the editor must abort the panel request");
      assert.equal(await page.locator(`iframe[src*="${tokens[1]}"]`).count(), 0);
    });

    test(`${engine} ${lang}: hide and reopen invalidates delayed panel history publications`, async t => {
      const { page, state } = await mounted(t, engine, lang);
      await page.evaluate(() => show("agents"));
      const panel = await ready(page, tokens[1]);
      await panel.evaluate(() => magpie.history.ready);
      for (const operation of ["list", "read", "media"]) {
        state.delays.set(operation, []);
        const arrived = page.waitForRequest(req => req.url().endsWith("/api/plugin-pages/history") && req.postDataJSON()?.operation === operation);
        await panel.evaluate(operation => {
          const selected = { source:"history", viewRef:"view", ...(operation === "list" ? {rootRef:"root"} : {sessionRef:"session", ...(operation === "media" ? {readRef:"read",mediaRef:"media"} : {})}) };
          window.pendingHistory = magpie.history[operation](selected).then(() => "published", e => e.message);
        }, operation);
        await arrived;
        await page.evaluate(() => show("extension-composite"));
        await page.evaluate(() => show("agents"));
        for (const release of state.delays.get(operation)) release();
        state.delays.delete(operation);
        assert.match(await panel.evaluate(() => pendingHistory), /changed|unavailable/, operation + " started before hiding must not publish after reopening");
      }
    });
  }
}
