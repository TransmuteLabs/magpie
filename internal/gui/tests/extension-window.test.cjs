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

async function fixture(page, lang, web) {
  const state = { generation: 0, enabled: true, unknown: false, calls: [], configs: [], options: [], holds: [], canceled: [], version: 0, notices: [] };
  const entry = (panel) => {
    const tok = tokens[state.generation * 2 + (panel ? 1 : 0)];
    return { spec: "sdk:composite", name: "composite", key: panel ? "extension-composite-0" : "extension-composite",
      title: "Composite", ref: tok, reads: ["providers:list"], slot: panel ? "agents" : undefined,
      url: `/_magpie/plugin/${tok}/index.html`, service: "_magpie/service/page.service/" };
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
      return json(state.enabled ? [entry(u.pathname.endsWith("panels"))] : []);
    }
    if (u.pathname === "/api/plugin-pages/changes") {
      if (u.searchParams.get("version") === String(state.version)) await new Promise((resolve) => state.notices.push(resolve));
      return json({ version: String(state.version) }).catch(() => {});
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
      if (!state.enabled || ![entry(false).ref, entry(true).ref].includes(tok)) return route.fulfill({ status: 404, body: "" });
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

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    test(`${engine} ${lang}: SDK bridge authority and generation disposal`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      for (const web of [false, true]) {
        await t.test(web ? "browser" : "desktop transport", async () => {
          const context = await browser.newContext({ viewport: { width: web ? 420 : 1100, height: 700 }, reducedMotion: "reduce" });
          const page = await context.newPage();
          page.setDefaultTimeout(8000);
          const errors = [];
          page.on("pageerror", (e) => errors.push(e.message));
          const state = await fixture(page, lang, web);
          try {
            const frame = await ready(page, tokens[0]);
            assert.equal(await frame.evaluate(() => magpie.service), `http://magpie.test/_magpie/plugin/${tokens[0]}/_magpie/service/page.service/`);
            assert.deepEqual(await frame.evaluate(() => magpie.backend.call("refresh", { value: 3 })), { binding: "refresh", input: { value: 3 } });
            // Malicious fields are dropped by the parent; the frame's owner
            // remains the entry the parent got from /api/plugin-pages.
            await frame.evaluate(() => new Promise((resolve) => {
              addEventListener("message", function reply(e) {
                if (e.data?.id !== 999) return;
                removeEventListener("message", reply); resolve(e.data.result);
              });
              parent.postMessage({ magpie: 1, id: 999, method: "backend.call", params: { ref: "forged", module: "other", scope: { account: "secret" }, url: "http://external/", binding: "refresh", input: { direct: true } } }, "*");
            }));
            const { request, ...forged } = state.calls[1];
            assert.deepEqual(forged, { ref: tokens[0], binding: "refresh", input: { direct: true } });
            assert.match(request, /^[a-f0-9]{32}:999$/);
            assert.equal(request.split(":")[0], state.calls[0].request.split(":")[0], "one parent nonce belongs to this frame");
            await frame.evaluate(() => new Promise((resolve) => {
              addEventListener("message", function reply(e) {
                if (e.data?.id !== 999) return;
                removeEventListener("message", reply); resolve();
              });
              parent.postMessage({ magpie: 1, id: 999, method: "backend.call", params: { request: "forged", binding: "refresh", input: { direct: true } } }, "*");
            }));
            assert.equal(state.calls[2].request, request, "duplicate delivery uses the same parent-owned operation identity");
            assert.match(await frame.evaluate(() => magpie.backend.call("forbidden", {}).catch((e) => e.message)), /binding not declared/);
            assert.deepEqual(await frame.evaluate(() => magpie.config.get()), { version: "0", values: { enabled: true } });
            assert.deepEqual(await frame.evaluate(() => magpie.config.cas("0", { flag: false })), { version: "1", values: { flag: false } });
            assert.match(await frame.evaluate(() => magpie.config.cas("stale", {}).catch((e) => e.message)), /configuration conflict/);
            assert.equal(await frame.evaluate(() => magpie.options.get().then(() => "allowed", () => "refused")), "refused");
            assert.equal(state.options.length, 0, "SDK config never touches legacy plugins.json");

            // Panel authority uses its own filtered-index ref.
            await page.evaluate(() => show("agents"));
            const panel = await ready(page, tokens[1]);
            await panel.evaluate(() => magpie.backend.call("panel.read", null));
            assert.equal(state.calls.at(-1).ref, tokens[1]);
            assert.notEqual(state.calls.at(-1).request.split(":")[0], request.split(":")[0], "panels have a separate parent nonce");

            state.unknown = true;
            await page.evaluate(() => loadPluginPages());
            assert.equal(await page.locator(`iframe[src*="${tokens[1]}"]`).count(), 1, "unknown metadata preserves the existing panel");
            state.unknown = false;
            await page.evaluate(() => show("extension-composite"));
            const held = page.waitForRequest((req) => req.url().endsWith("/api/plugin-pages/call") && req.postDataJSON()?.binding === "hold");
            await frame.evaluate(() => { window.held = magpie.backend.call("hold", {}).catch(() => null); });
            await held;
            state.generation = 1;
            state.notify();
            const replacement = await ready(page, tokens[2]);
            assert.equal(await page.locator(`iframe[src*="${tokens[0]}"]`).count(), 0, "update disposes the old page");
            assert.equal(await page.locator(`iframe[src*="${tokens[1]}"]`).count(), 0, "update disposes old panels");
            await replacement.evaluate(() => magpie.backend.call("refresh", {}));
            assert.equal(state.calls.at(-1).ref, tokens[2], "new calls use the new parent-owned ref");
            assert.notEqual(state.calls.at(-1).request.split(":")[0], request.split(":")[0], "a recreated frame gets a new nonce");
            assert.ok(state.canceled.some((b) => b.binding === "hold"), "update aborts an in-flight backend fetch");
            state.enabled = false;
            state.notify();
            await page.locator('#nav button[data-view="extension-composite"]').waitFor({ state: "detached" });
            assert.equal(await page.locator(`iframe[src*="${tokens[2]}"]`).count(), 0, "disable disposes the active frame");
            state.enabled = true;
            state.generation = 2;
            state.notify();
            await page.locator('#nav button[data-view="extension-composite"]').waitFor({ state: "attached" });
            await page.evaluate(() => show("extension-composite"));
            await ready(page, tokens[4]);
            assert.equal(await page.locator("#view-extension-composite").count(), 1, "re-enable reuses its kernel slot");
            assert.deepEqual(errors, []);
          } finally {
            for (const resolve of [...state.holds, ...state.notices]) resolve();
            await context.close();
          }
        });
      }
    });
  }
}
