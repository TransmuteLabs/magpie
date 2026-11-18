const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { watchSettingsReads, settingsDrawn, wheelTo } = require("./reader.cjs");
const { isPageLifecycleRequest } = require("./fixtures/page-lifecycle.cjs");

const assets = process.env.MAGPIE_CAP_ASSETS || path.resolve(__dirname, "../assets");
const script = process.env.MAGPIE_CAP_SETTINGS || path.join(assets, "plugins/gateway/settings.js");
const words = {
  en: { all: "Every model", own: "Models without their own", bad: "Longest context: lots is not a number of tokens like 272k" },
  zh: { all: "每个模型", own: "没有自带窗口的模型", bad: "最长上下文：lots 不是 272k 这样的 token 数" },
};

function fixture(lang, initial, failFirst, holdMode = false) {
  let settings = {
    theme: "light", lang, tray: "panel", currency: "usd", version: "0.1.900",
    dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425",
    proxyNow: "none", proxySource: "none", visionModels: [], imageGenModels: [],
    titleModels: [], workbuddyCheckins: [], lanURLs: [], redactWords: [], trayUsageEvery: 3,
    contextCap: initial,
  };
  let release, first = true;
  const gate = new Promise((resolve) => { release = resolve; });
  const posts = [], unexpected = [];
  return {
    posts, unexpected, release,
    async route(route) {
      const request = route.request(), url = new URL(request.url());
      const json = (data) => route.fulfill({ json: data });
      if (isPageLifecycleRequest(request)) return json({});
      if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:true};` });
      if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
      if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
      if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true } });
      if (url.pathname === "/api/settings/context-cap") {
        const body = request.postDataJSON();
        posts.push(["context-cap", body]);
        const held = first && !holdMode;
        if (!holdMode) first = false;
        if (!held || !failFirst) settings = { ...settings, contextCap: body.at || undefined };
        const snapshot = { ...settings };
        if (held) await gate;
        if (held && failFirst) return route.fulfill({ status: 500, json: { error: "fixture cap denied" } });
        return json(snapshot);
      }
      if (url.pathname === "/api/settings/context-cap-mode") {
        const body = request.postDataJSON();
        posts.push(["context-cap-mode", body]);
        const held = first && holdMode;
        if (holdMode) first = false;
        if (!held || !failFirst) settings = { ...settings, contextCapAll: body.all || undefined };
        const snapshot = { ...settings };
        if (held) await gate;
        if (held && failFirst) return route.fulfill({ status: 500, json: { error: "fixture mode denied" } });
        return json(snapshot);
      }
      if (url.pathname === "/api/settings") return json(settings);
      if (url.pathname === "/api/plugins") return json({ plugins: [] });
      if (url.pathname === "/api/usage/quotas") return json([]);
      if (url.pathname === "/api/agents/cli") return json({ agents: {}, pending: false });
      if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
      if (url.pathname.startsWith("/api/")) {
        if (request.method() !== "GET") unexpected.push([url.pathname, request.method(), request.postData()]);
        return json({});
      }
      const file = url.pathname === "/plugins/gateway/settings.js" ? script : path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
      try {
        return await route.fulfill({ body: await fs.readFile(file), contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)] });
      } catch (error) {
        if (error.code !== "ENOENT") throw error;
        return route.fulfill({ status: 404, body: "" });
      }
    },
  };
}

for (const engine of ["chromium", "webkit"]) {
  for (const lang of ["en", "zh"]) {
    for (const failFirst of [false, true]) {
      test(`${engine} ${lang}: context mode holds ${failFirst ? "error" : "success"}`, { timeout: 45000 }, async (t) => {
        const upstream = fixture(lang, 0, failFirst, true);
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(async () => { upstream.release(); await browser.close(); });
        const page = await browser.newPage({ viewport: { width: 900, height: 420 }, reducedMotion: "reduce" });
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.route("**/*", upstream.route);
        await watchSettingsReads(page);
        await page.goto("http://magpie.test/?view=settings&tab=models");
        await settingsDrawn(page);
        const field = page.locator("#contextCapRow input.context-cap");
        const mode = page.locator("#contextCapRow button.context-cap-mode");
        await wheelTo(page, field, 160);
        const scroll = () => page.locator("#view-settings").evaluate((view) => view.scrollTop);
        const before = await scroll();
        await mode.click();
        await page.locator(".context-cap-menu [role=menuitemradio], .context-cap-menu button").filter({ hasText: words[lang].all }).first().click();
        await page.waitForFunction(() => prefsBusy === 1);
        const initiallyDisabled = await mode.isDisabled();
        assert.equal(await field.isEnabled(), true, "mode write does not block the independent cap");
        await field.fill("272k");
        await field.press("Enter");
        await page.waitForFunction(() => prefs.contextCap === 272000 && prefsBusy === 1);
        const afterRedrawDisabled = await mode.isDisabled();
        upstream.release();
        if (failFirst) await page.getByText("fixture mode denied", { exact: true }).waitFor();
        await page.waitForFunction(() => !prefsBusy);
        t.diagnostic(JSON.stringify({ phase: "late-mode-response", posts: upstream.posts, cap: await field.inputValue(), mode: await mode.textContent(), initiallyDisabled, afterRedrawDisabled }));
        assert.equal(await field.inputValue(), "272k", "late mode reply must preserve the newer explicit cap");
        assert.equal(initiallyDisabled, true, "mode owns its pending input synchronously");
        assert.equal(afterRedrawDisabled, true, "mode pending ownership survives cap redraw");
        assert.equal(await mode.isEnabled(), true, "mode success/error releases pending ownership");
        assert.equal((await mode.textContent()).trim(), failFirst ? words[lang].own : words[lang].all);
        if (failFirst) {
          await mode.click();
          await page.locator(".context-cap-menu [role=menuitemradio], .context-cap-menu button").filter({ hasText: words[lang].all }).first().click();
          await page.waitForFunction(() => !prefsBusy && prefs.contextCapAll === true);
        }
        await field.fill("off");
        await field.press("Enter");
        await page.waitForFunction(() => !prefsBusy && document.querySelector("#contextCapRow input")?.value === "");
        const wanted = [["context-cap-mode", { all: true }], ["context-cap", { at: 272000 }]];
        if (failFirst) wanted.push(["context-cap-mode", { all: true }]);
        wanted.push(["context-cap", { at: 0 }]);
        assert.deepEqual(upstream.posts, wanted);
        assert.equal((await mode.textContent()).trim(), words[lang].all);
        assert.equal(await scroll(), before);
        assert.deepEqual(upstream.unexpected, []);
        assert.deepEqual(errors, []);
      });
    }
  }
}

for (const engine of ["chromium", "webkit"]) {
  for (const lang of ["en", "zh"]) {
    for (const scenario of ["held-success", "held-redraw", "held-error", "ready-control"]) {
      test(`${engine} ${lang}: context cap ${scenario}`, { timeout: 45000 }, async (t) => {
        const initial = scenario === "held-error" ? 128000 : 0;
        const upstream = fixture(lang, initial, scenario === "held-error");
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(async () => { upstream.release(); await browser.close(); });
        const page = await browser.newPage({ viewport: { width: 900, height: 420 }, reducedMotion: "reduce" });
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        await page.route("**/*", upstream.route);
        await watchSettingsReads(page);
        await page.goto("http://magpie.test/?view=settings&tab=models");
        await settingsDrawn(page);
        const field = page.locator("#contextCapRow input.context-cap");
        const mode = page.locator("#contextCapRow button.context-cap-mode");
        await wheelTo(page, field, 160);
        const scroll = () => page.locator("#view-settings").evaluate((view) => view.scrollTop);
        const before = await scroll();
        if (scenario === "ready-control") {
          await field.fill("lots");
          await field.press("Enter");
          await page.getByText(words[lang].bad, { exact: true }).waitFor();
          assert.deepEqual(upstream.posts, []);
          upstream.release();
        }
        await field.fill("272k");
        await field.press("Enter");
        if (scenario !== "ready-control") {
          await page.waitForFunction(() => prefsBusy === 1);
          t.diagnostic(JSON.stringify({ phase: "held", posts: upstream.posts, disabled: await field.isDisabled(), input: await field.inputValue() }));
          assert.equal(await field.isDisabled(), true, "pending cap input must reject a stale second edit");
          assert.deepEqual(upstream.posts, [["context-cap", { at: 272000 }]]);
          if (scenario === "held-redraw" || scenario === "held-error") {
            assert.equal(await mode.isEnabled(), true, "unrelated mode remains interactive");
            await mode.click();
            await page.locator(".context-cap-menu [role=menuitemradio], .context-cap-menu button").filter({ hasText: words[lang].all }).first().click();
            await page.waitForFunction((text) => document.querySelector("#contextCapRow .context-cap-mode")?.textContent.trim() === text, words[lang].all);
            assert.equal(await field.isDisabled(), true, "pending ownership survives an unrelated redraw");
            assert.deepEqual(upstream.posts.at(-1), ["context-cap-mode", { all: true }]);
          }
          upstream.release();
        }
        if (scenario === "held-error") {
          await page.getByText("fixture cap denied", { exact: true }).waitFor();
          await page.waitForFunction(() => !prefsBusy && document.querySelector("#contextCapRow input")?.value === "128k");
          assert.equal(await field.isEnabled(), true, "failed save releases pending ownership");
          assert.equal((await mode.textContent()).trim(), words[lang].all);
          await field.fill("272k");
          await field.press("Enter");
        }
        await page.waitForFunction(() => !prefsBusy && prefs.contextCap === 272000 && document.querySelector("#contextCapRow input")?.value === "272k");
        assert.equal(await field.isEnabled(), true, "successful save releases pending ownership");
        if (scenario === "held-redraw") assert.equal((await mode.textContent()).trim(), words[lang].all, "late cap reply must preserve the newer explicit mode");
        const count = upstream.posts.length;
        await field.fill("272k");
        await field.press("Enter");
        assert.equal(upstream.posts.length, count, "unchanged explicit value creates no request");
        await field.fill("off");
        await field.press("Enter");
        await page.waitForFunction(() => !prefsBusy && document.querySelector("#contextCapRow input")?.value === "");
        const wanted = [["context-cap", { at: 272000 }]];
        if (scenario === "held-redraw" || scenario === "held-error") wanted.push(["context-cap-mode", { all: true }]);
        if (scenario === "held-error") wanted.push(["context-cap", { at: 272000 }]);
        wanted.push(["context-cap", { at: 0 }]);
        assert.deepEqual(upstream.posts, wanted);
        assert.equal(await scroll(), before, "pending success/error and edits must not scroll");
        assert.deepEqual(upstream.unexpected, []);
        assert.deepEqual(errors, []);
        t.diagnostic(JSON.stringify({ phase: "terminal", posts: upstream.posts, scroll: await scroll() }));
      });
    }
  }
}
