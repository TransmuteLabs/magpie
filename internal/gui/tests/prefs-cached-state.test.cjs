const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");
const { reach } = require("./reader.cjs");
const { isPageLifecycleRequest } = require("./fixtures/page-lifecycle.cjs");

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

for (const engine of ["chromium", "webkit"]) for (const saving of [true, false]) {
  test(`${engine}: late cached state ${saving ? "preserves accepted theme" : "bootstraps without a write"}`, { timeout: 45000 }, async (context) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    const cachedGate = deferred(), freshGate = deferred(), cachedAsked = deferred();
    context.after(async () => { cachedGate.resolve(); freshGate.resolve(); await browser.close(); });
    const page = await browser.newPage({ viewport: { width: 1100, height: 850 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(6000);
    page.setDefaultNavigationTimeout(60000);
    let settings = {
      theme: "light", lang: "en", tray: "panel", currency: "usd", version: "0.1.900",
      dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425",
      visionModels: [], imageGenModels: [], titleModels: [], workbuddyCheckins: [],
      lanURLs: [], redactWords: [], trayUsageEvery: 3,
    };
    const posts = [], unexpected = [], pageErrors = [], routeErrors = [];
    const fallback = fixture("en", "light", [], { keys: [] });
    page.on("pageerror", (error) => pageErrors.push(error.message));
    await page.route("**/*", async (route) => {
      try {
        const request = route.request(), url = new URL(request.url());
        if (isPageLifecycleRequest(request)) return route.fulfill({ json: {} });
        if (url.pathname === "/api/state") {
          assert.equal(request.method(), "GET");
          const cached = url.searchParams.has("last");
          const snapshot = {
            last: cached, agents: [], profiles: [], catalog: cached ? "cached" : "fresh",
            settings: { ...settings, theme: saving ? "light" : "dark" },
          };
          if (cached) cachedAsked.resolve();
          await (cached ? cachedGate.promise : freshGate.promise);
          return route.fulfill({ json: snapshot });
        }
        if (url.pathname === "/api/settings") {
          if (request.method() === "POST") {
            const body = request.postDataJSON();
            posts.push(body);
            settings = { ...settings, ...body };
          }
          return route.fulfill({ json: settings });
        }
        if (request.method() !== "GET" && url.pathname.startsWith("/api/")) {
          unexpected.push({ path: url.pathname, body: request.postData() });
        }
        return await fallback(route);
      } catch (error) {
        routeErrors.push(error.message);
        await route.abort().catch(() => {});
      }
    });
    await page.goto("http://magpie.test/?view=agents");
    await cachedAsked.promise;
    assert.equal(await page.evaluate(() => !!load.done), false);
    await page.locator("#prefs").click();
    await page.waitForFunction(() => view === "settings" && prefs?.theme === "light");
    if (saving) {
      const dark = page.locator('#themeSegs button[data-seg="dark"]');
      await reach(page, dark);
      await dark.click();
      await page.waitForFunction(() => !prefsBusy && prefs?.theme === "dark" && state.settings.theme === "dark");
      assert.equal(posts.length, 1);
      assert.equal(posts[0].theme, "dark");
    }
    cachedGate.resolve();
    await page.waitForFunction(() => state.last && state.catalog === "cached");
    const observed = await page.evaluate(() => ({
      stateTheme: state.settings.theme, painted: document.documentElement.dataset.theme,
      selected: document.querySelector("#themeSegs .opt.on")?.dataset.seg,
      prefsTheme: prefs.theme, firstFreshDone: !!load.done,
    }));
    assert.equal(observed.stateTheme, "dark");
    assert.equal(observed.painted, "dark");
    assert.equal(observed.firstFreshDone, false, "a later fresh read cannot conceal cached rollback");
    assert.equal(observed.prefsTheme, saving ? "dark" : "light");
    assert.equal(observed.selected, saving ? "dark" : "light");
    assert.equal(posts.length, saving ? 1 : 0);
    assert.deepEqual(unexpected, []);
    assert.deepEqual(pageErrors, []);
    assert.deepEqual(routeErrors, []);
  });
}
