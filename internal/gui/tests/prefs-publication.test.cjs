const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { fixture } = require("./fixtures/caller-keys.cjs");
const { watchSettingsReads, settingsDrawn, wheelTo, openPart } = require("./reader.cjs");

for (const engine of ["chromium", "webkit"]) for (const saving of [true, false]) {
  test(`${engine}: cached Agents ${saving ? "settings save" : "unchanged settings"} precedes first fresh state`, { timeout: 45000 }, async (testContext) => {
    let settings = {
      theme: "light", lang: "en", tray: "panel", currency: "usd",
      visionModels: [], imageGenModels: [], titleModels: [], workbuddyCheckins: [],
      lanURLs: [], redactWords: [], dir: "~/.config/magpie", version: "0.1.900",
      gateway: "http://127.0.0.1:3425",
    };
    const posts = [], errors = [];
    let releaseState, releaseRead, maskingReads = 0, firstReleased = false;
    const stateGate = new Promise((resolve) => { releaseState = resolve; });
    const readGate = new Promise((resolve) => { releaseRead = resolve; });
    const cached = { last: true, agents: [], profiles: [], catalog: "cached", settings: { ...settings } };
    const fresh = { agents: [], profiles: [], catalog: "fresh", settings: { ...settings } };
    const fallback = fixture("en", "light", [], { keys: [] });
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    testContext.after(async () => { releaseState(); releaseRead(); await browser.close(); });
    const page = await browser.newPage({ viewport: { width: 900, height: 700 }, reducedMotion: "reduce" });
    page.setDefaultTimeout(6000);
    page.setDefaultNavigationTimeout(60000);
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      if (url.pathname === "/api/state") {
        if (url.searchParams.get("last")) return route.fulfill({ json: cached });
        await stateGate;
        return route.fulfill({ json: fresh });
      }
      if (url.pathname === "/api/settings") {
        if (request.method() === "POST") {
          const body = request.postDataJSON();
          posts.push(body);
          settings = { ...settings, ...body };
        } else if (firstReleased) {
          maskingReads++;
          await readGate;
        }
        return route.fulfill({ json: settings });
      }
      return fallback(route);
    });
    await watchSettingsReads(page);
    await page.goto("http://magpie.test/?view=agents");
    await page.waitForFunction(() => state.last && !load.done && view === "agents");
    await page.locator("#prefs").click();
    await page.waitForFunction(() => view === "settings" && prefs && !load.done && __settingsReads.asked === __settingsReads.drawn);
    if (saving) {
      const dark = page.locator("#themeSegs").getByRole("button", { name: "Dark", exact: true });
      await wheelTo(page, dark, 300);
      await dark.click();
      await page.waitForFunction(() => !prefsBusy && prefs.theme === "dark" && document.documentElement.dataset.theme === "dark");
      assert.equal(posts.length, 1);
      assert.equal(posts[0].theme, "dark");
    } else {
      settings.theme = fresh.settings.theme = "dark";
      assert.equal(posts.length, 0);
    }
    assert.equal(await page.evaluate(() => !!load.done), false);
    firstReleased = true;
    releaseState();
    await page.waitForFunction(() => load.done && state.catalog === "fresh");
    assert.equal(maskingReads, 1, "the post-bootstrap Settings read is held before it can hide a rollback");
    assert.deepEqual(await page.evaluate(() => ({
      prefs: prefs.theme, state: state.settings.theme, painted: document.documentElement.dataset.theme,
      catalog: state.catalog,
    })), { prefs: saving ? "dark" : "light", state: "dark", painted: "dark", catalog: "fresh" },
    "first fresh state must preserve the accepted settings while publishing other bootstrap fields");
    releaseRead();
    await settingsDrawn(page);
    assert.equal(await page.locator("#themeSegs button.on").textContent(), "Dark");
    assert.deepEqual(errors, []);
  });
}

for (const engine of ["chromium", "webkit"]) {
  for (const scenario of ["late full settings", "failed authority read", "About community lifecycle"]) {
    test(`${engine}: ${scenario}`, { timeout: 45000 }, async (testContext) => {
      let settings = {
        theme: "light", lang: "en", tray: "panel", currency: "usd",
        contextCap: 128000, contextCapAll: true, trayUsageEvery: 3,
        visionModels: [], imageGenModels: [], titleModels: [], workbuddyCheckins: [],
        lanURLs: [], redactWords: [], dir: "~/.config/magpie", log: "/fixture/magpie.log", version: "0.1.900",
        gateway: "http://127.0.0.1:3425",
      };
      const posts = [], opened = [], revealed = [], errors = [];
      let authorityError = false;
      let release;
      const gate = new Promise((resolve) => { release = resolve; });
      const fallback = fixture("en", "light", [], { keys: [] });
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      testContext.after(async () => { release(); await browser.close(); });
      const page = await browser.newPage({ viewport: { width: 900, height: 700 }, reducedMotion: "reduce" });
      page.setDefaultTimeout(6000);
      page.setDefaultNavigationTimeout(60000);
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", async (route) => {
        const request = route.request(), url = new URL(request.url());
        if (url.pathname === "/api/open") {
          opened.push(request.postDataJSON());
          return route.fulfill({ json: {} });
        }
        if (["/api/settings/reveal", "/api/settings/log/reveal"].includes(url.pathname)) {
          revealed.push(url.pathname);
          return route.fulfill({ json: {} });
        }
        if (url.pathname === "/api/notices") return route.fulfill({ json: { text: "Fixture third-party notices" } });
        if (url.pathname === "/updatesource.js" && scenario === "About community lifecycle") await gate;
        if (url.pathname === "/api/settings" && request.method() === "GET") {
          return authorityError
            ? route.fulfill({ status: 500, json: { error: "fixture authority unavailable" } })
            : route.fulfill({ json: settings });
        }
        if (["/api/settings/busy-load", "/api/settings/context-cap"].includes(url.pathname)) {
          assert.equal(request.method(), "POST");
          const body = request.postDataJSON();
          posts.push({ path: url.pathname, body });
          if (url.pathname.endsWith("busy-load")) settings = { ...settings, busyLoad: body.at };
          else {
            settings = { ...settings };
            delete settings.contextCap;
            if (body.at) settings.contextCap = body.at;
          }
          const snapshot = JSON.parse(JSON.stringify(settings));
          if (url.pathname.endsWith("busy-load")) await gate;
          return route.fulfill({ json: snapshot });
        }
        return fallback(route);
      });
      await watchSettingsReads(page);
      await page.goto("http://magpie.test/?view=settings&tab=models");
      await settingsDrawn(page);
      if (scenario !== "About community lifecycle") {
        const busy = page.locator("#busyLoadSegs button").filter({ hasText: "Over 50%" });
        const cap = page.locator("#contextCapRow input.context-cap");
        await wheelTo(page, busy, 240);
        await busy.click();
        await page.waitForFunction(() => prefsBusy === 1);
        await wheelTo(page, cap, 360);
        await cap.fill("off");
        await cap.press("Enter");
        await page.waitForFunction(() => prefsBusy === 1 && !prefs.contextCap);
        assert.equal(await cap.inputValue(), "");
        authorityError = scenario === "failed authority read";
        release();
        await page.waitForFunction(() => !prefsBusy);
        assert.equal(await cap.inputValue(), "", "late sibling must preserve an acknowledged cap clear");
        assert.equal(settings.contextCap, undefined);
        if (authorityError) await page.locator("#status", { hasText: "fixture authority unavailable" }).waitFor();
        assert.deepEqual(posts, [
          { path: "/api/settings/busy-load", body: { at: 50 } },
          { path: "/api/settings/context-cap", body: { at: 0 } },
        ]);
      } else {
        await openPart(page, "about");
        await page.locator("#about").getByText("Community", { exact: true }).waitFor();
        release();
        for (let redraw = 0; redraw < 2; redraw++) {
          const discord = page.locator("#about button.discord"), github = page.locator("#about button.github");
          await discord.waitFor();
          assert.equal(await discord.count(), 1);
          assert.equal(await github.count(), 1);
          assert.equal((await discord.textContent()).trim(), "Join Discord");
          assert.equal((await github.textContent()).trim(), "GitHub");
          assert.equal(await discord.locator("svg path").count(), 1);
          assert.equal(await github.locator("svg path").count(), 1);
          await wheelTo(page, discord, 360);
          await discord.click();
          await github.click();
          const aboutRow = (name) => page.locator("#about .row.pref").filter({ has: page.locator(".name").getByText(name, { exact: true }) });
          for (const name of ["Config folder", "Log", "Website"]) {
            const open = aboutRow(name).getByRole("button", { name: "Open", exact: true });
            await wheelTo(page, open, 360);
            await open.click();
          }
          const licenses = aboutRow("Open-source licenses").getByRole("button", { name: "View", exact: true });
          assert.equal(await licenses.count(), 1);
          await wheelTo(page, licenses, 360);
          await licenses.click();
          await page.locator("#modal .notices-text", { hasText: "Fixture third-party notices" }).waitFor();
          await page.locator("#modal").getByRole("button", { name: "Done", exact: true }).click();
          await page.locator("#modal").waitFor({ state: "hidden" });
          await page.waitForFunction(() => !prefsBusy);
          await page.evaluate(() => loadSettings());
        }
        await page.waitForFunction(() => !prefsBusy);
        assert.deepEqual(opened, [
          { url: "https://discord.gg/vGSnD3ZKQF" }, { url: "https://github.com/yetone/magpie" },
          { url: "https://usemagpie.ai" },
          { url: "https://discord.gg/vGSnD3ZKQF" }, { url: "https://github.com/yetone/magpie" },
          { url: "https://usemagpie.ai" },
        ]);
        assert.deepEqual(revealed, ["/api/settings/reveal", "/api/settings/log/reveal", "/api/settings/reveal", "/api/settings/log/reveal"]);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      }
      assert.deepEqual(errors, []);
    });
  }
}
