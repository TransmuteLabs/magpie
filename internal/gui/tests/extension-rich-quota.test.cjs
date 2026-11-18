const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const reset = new Date(Date.now() + 3600e3).toISOString();
const quotas = [{
  provider: "extension-fixture", name: "SDK fixture", user: "synthetic", windows: [
    { name: "Exact zero", used: 0, amount: 0, limit: 10, unit: "ACU", resetsAt: reset },
    { name: "Missing amount", used: 25, limit: 10, unit: "ACU" },
    { name: "Wallet", used: 0, text: true, unknown: true, balance: 0, unit: "ACU" },
    { name: "Gift", used: 0, text: true, unknown: true, display: "Gift pending" },
    { name: "Count", used: 25, amount: 2.5, limit: 10, unit: "ACU" },
  ],
}];

function serve(lang, left) {
  const settings = { lang, theme: "light", quotaLeft: left, currency: "usd" };
  return async route => {
    const url = new URL(route.request().url());
    const json = body => route.fulfill({ json: body });
    if (url.pathname === "/boot.js") return route.fulfill({
      contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};`,
    });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (url.pathname === "/api/settings") return json(settings);
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/usage/quotas") return json(quotas);
    if (url.pathname === "/api/usage/quotas/history") return json([]);
    if (url.pathname === "/api/usage") return json({ days: [] });
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { await route.fulfill({ body: await fs.readFile(file), contentType }); }
    catch { await route.fulfill({ status: 404, body: "" }); }
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    test(`${engine} ${lang}: SDK quota presence and wallet text reach ordinary cards`, async context => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      context.after(() => browser.close());
      for (const left of [false, true]) {
        const page = await browser.newPage({ viewport: { width: 900, height: 800 }, reducedMotion: "reduce" });
        page.setDefaultTimeout(10000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", error => errors.push(error.message));
        await page.route("**/*", serve(lang, left));
        await page.goto("http://magpie.test/?view=usage&tab=usage");
        const card = page.locator(".subscription-card", { hasText: "SDK fixture" });
        const values = card.locator(".quota-n");
        await values.nth(4).waitFor();
        const text = await values.allTextContents();
        assert.match(text[0], new RegExp(`^${left ? "10" : "0"} / 10 ACU`));
        assert.ok(!text[1].includes("/") && !text[1].includes("ACU"), "missing amount must not invent zero or a count");
        assert.ok(text[2].includes("0 ACU") && !text[2].includes("%") && !text[2].includes("$"), text[2]);
        assert.equal(text[3], "Gift pending");
        assert.match(text[4], new RegExp(`^${left ? "7.5" : "2.5"} / 10 ACU`));
        const resetLine = card.locator(".quota-reset").first();
        assert.ok((await resetLine.textContent()).trim(), "vendor reset must remain visible");
        assert.deepEqual(errors, []);
        await page.close();
      }
    });
  }
}
