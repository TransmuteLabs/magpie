// Existing provider/confirmation controls, with ZCode's two independent
// pools. The API is synthetic; no vendor account or real credit is used.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const user = "reset@example.com";
const context = "a".repeat(64);
const later = new Date(Date.now() + 864e5).toISOString();

function fixture(lang) {
  const state = { posts: [], phase: 0 };
  const quota = () => ({ provider: "zcode", user, plan: "Team", icon: "zcode", windows: [
    { name: "5 hours", used: 50, resetsAt: later },
    { name: "7 days", used: state.phase === 2 ? 0 : 100, resetsAt: later },
  ], resets: { byWindow: true, count: state.phase === 2 ? 1 : 2, fiveHour: 1, weekly: state.phase === 2 ? 0 : 1,
    team: true, canUse: true, context, ...(state.phase === 1 ? { pending: "weekly" } : {}) } });
  state.serve = async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light", quotaLeft: true } });
    if (url.pathname === "/api/settings") return json({ lang, theme: "light", quotaLeft: true });
    if (url.pathname === "/api/providers") return json({ providers: [{ id: "zcode", name: "ZCode", icon: "zcode", models: [{ id: "GLM-5.3", on: true }], agents: [],
      account: { agent: "zcode", user, plan: "Team", logins: [{ user, plan: "Team", active: true, on: true }] } }], presets: [], excluded: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/login/usage") return json({ [user]: quota() });
    if (url.pathname === "/api/usage/quotas") return json([quota()]);
    if (url.pathname === "/api/usage/zcode-reset") {
      state.posts.push(req.postDataJSON());
      state.phase++;
      return json({ code: state.phase === 1 ? "reset_unconfirmed" : "reset", windows: state.phase === 1 ? 0 : 1 });
    }
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType });
  };
  return state;
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const width of [420, 1000]) {
      test(`${engine} ${lang} ${width}px: ZCode resets bind one window and retry only the pending operation`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const page = await (await browser.newContext({ viewport: { width, height: 800 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(5000);
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        const state = fixture(lang);
        await page.route("**/*", state.serve);
        await page.goto("http://magpie.test/?view=providers");
        await page.locator('.row.provider[data-id="zcode"]').click();
        const chip = page.locator(`.editor .acc[data-account-id="${user}"] .aq-resets`);
        await chip.waitFor();
        assert.equal(await chip.getAttribute("role"), "button");
        const words = await page.evaluate(() => ({ title: t("Use a ZCode reset?"), cancel: t("Cancel"), use: t("Use a reset"), weekly: t("Weekly reset"), five: t("Five-hour reset"), pending: t("The reset may have been used. Refresh or retry the same operation before spending another reset.") }));
        const top = await page.evaluate(() => document.scrollingElement.scrollTop);
        await chip.click();
        const ask = page.locator(".reset-ask");
        await ask.waitFor();
        assert.equal((await ask.locator(".ehead b").textContent()).trim(), words.title);
        assert.ok((await ask.textContent()).includes(user));
        assert.equal(await ask.locator("select").count(), 0);
        await ask.getByRole("button", { name: words.cancel, exact: true }).click();
        await ask.waitFor({ state: "detached" });
        assert.deepEqual(state.posts, []);
        assert.equal(await page.evaluate(() => document.scrollingElement.scrollTop), top);

        await chip.click();
        await ask.waitFor();
        await ask.locator(".reset-card").click();
        const menu = page.locator(".proto-menu");
        await menu.waitFor();
        await menu.locator(".pm-item").filter({ hasText: words.weekly }).click();
        assert.equal((await ask.locator(".reset-card").textContent()).trim(), words.weekly);
        await ask.getByRole("button", { name: words.use, exact: true }).click();
        await ask.waitFor({ state: "detached" });
        assert.deepEqual(state.posts, [{ provider: "zcode", user, window: "weekly", context }]);
        await page.locator("#status", { hasText: words.pending }).waitFor();

        await chip.waitFor();
        await chip.click();
        await ask.waitFor();
        assert.equal((await ask.locator(".reset-card").textContent()).trim(), words.weekly, "retry changed the pending window");
        await ask.locator(".reset-card").click();
        await menu.waitFor();
        assert.deepEqual(await menu.locator(".pm-item .pm-name").allTextContents(), [words.weekly]);
        await page.keyboard.press("Escape");
        await ask.getByRole("button", { name: words.use, exact: true }).click();
        await ask.waitFor({ state: "detached" });
        assert.deepEqual(state.posts, [
          { provider: "zcode", user, window: "weekly", context },
          { provider: "zcode", user, window: "weekly", context },
        ]);
        await chip.waitFor();
        await chip.click();
        await ask.waitFor();
        assert.equal((await ask.locator(".reset-card").textContent()).trim(), words.five, "the confirmed week consumed the other pool");
        await ask.getByRole("button", { name: words.cancel, exact: true }).click();
        assert.equal(state.posts.length, 2);
        assert.deepEqual(errors, []);
      });
    }
  }
}
