// Run with Node's test runner and Playwright on the module path; see README.md.
// Update source (RootSir and Sakugar_ on X): Settings › About has a menu
// button right under Download source, with the other update settings, and
// Network no longer has it (WeltAn_X on X). Automatic, usemagpie.ai only and GitHub
// only each save updateSource alone (""/"site"/"github"), in the app's menu
// and in every language.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { openPart, reach, watchSettingsReads, settingsDrawn } = require("./reader.cjs");

const assets = path.resolve(__dirname, "../assets");
function server(lang, posts, signing = {}) {
  let pin = { supported: !!signing.supported, configured: false };
  let cur = {
    theme: "light", lang, tray: "panel", currency: "usd", textSize: 100,
    proxy: "direct", proxyNow: "", proxySource: "off", pluginProxy: "", updateProxy: "",
    trayUsageEvery: 3, version: "test", dir: "/test", lanURLs: [],
    visionModels: [], imageGenModels: [], workbuddyCheckins: [],
    fx: { rate: 7.2, at: new Date().toISOString(), stale: false },
  };
  return async (route) => {
    const req = route.request(), url = new URL(req.url());
    assert.equal(url.origin, "http://magpie.test", "every request belongs to the fixture");
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = ${JSON.stringify({ lang, theme: "light", web: true })};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" }, fx: cur.fx });
    if (url.pathname === "/api/settings") {
      if (req.method() === "POST") {
        const body = req.postDataJSON();
        posts.push(body);
        // as settings.Save: the custom copy is the server's, set from the value
        cur = { ...cur, ...body };
      }
      return json(cur);
    }
    if (url.pathname === "/api/update/signing") {
      signing.reads?.push(req.method() + url.search);
      if (req.method() === "POST") {
        const body = req.postDataJSON(); posts.push({ signing: body });
        if (signing.saveError) return route.fulfill({ status: 400, json: { error: signing.saveError } });
        pin = { supported: true, configured: !body.off, fingerprint: body.off ? undefined : body.fingerprint, team: "TEAM", feed: "https://fork.test/latest.json" };
      }
      if (url.searchParams.has("identities") && signing.listError) return route.fulfill({ status: 400, json: { error: signing.listError } });
      return json({ ...pin, identities: url.searchParams.has("identities") ? [
        { fingerprint: "A".repeat(40), name: "Same signing name", team: "TEAM" },
        { fingerprint: "B".repeat(40), name: "Same signing name", team: "TEAM" },
      ] : undefined });
    }
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/plugin-pages" || url.pathname === "/api/plugin-panels") return json([]);
    if (url.pathname === "/api/plugin-pages/changes") {
      if (url.searchParams.get("version") !== "empty") return json({ version: "empty" });
      return;
    }
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const labels = {
  en: { row: "Update source", auto: "Automatic", site: "usemagpie.ai only", github: "GitHub only", saved: "Saved" },
  zh: { row: "更新源", auto: "自动", site: "仅 usemagpie.ai", github: "仅 GitHub", saved: "已保存" },
  "zh-TW": { row: "更新來源", auto: "自動", site: "僅 usemagpie.ai", github: "僅 GitHub", saved: "已儲存" },
  ja: { row: "アップデート元", auto: "自動", site: "usemagpie.ai のみ", github: "GitHub のみ", saved: "保存しました" },
  de: { row: "Update-Quelle", auto: "Automatisch", site: "Nur usemagpie.ai", github: "Nur GitHub", saved: "Gespeichert" },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(labels)) {
    const l = labels[lang];
    for (const width of [1100, 420]) {
      test(`${engine} ${lang} ${width}px: the update source is picked from the app's menu`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const context = await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce", serviceWorkers: "block" });
        const page = await context.newPage();
        page.setDefaultTimeout(4000);
        page.setDefaultNavigationTimeout(60000);
        const posts = [], errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await context.route("**/*", server(lang, posts));
        await watchSettingsReads(page);
        const signingRead = page.waitForResponse((response) => response.url().endsWith("/api/update/signing"));
        await page.goto("http://magpie.test/?view=settings&tab=about");
        await settingsDrawn(page);
        await signingRead;
        const row = page.locator("#about > #updateSourceRow");
        await row.getByText(l.row, { exact: true }).waitFor();
        assert.equal(await page.locator("#updateSourceRow").count(), 1, "one Update source row");
        assert.equal(await page.locator("#setPage-network #updateSourceRow, #setPage-network #updateSourcePick").count(), 0, "not on Network");
        assert(await row.evaluate((r) => r.previousElementSibling?.classList.contains("update-mirror-row")), "right under Download source");
        assert.equal(await row.locator("select").count(), 0, "no native select");
        const pick = page.locator("#updateSourcePick");
        assert.equal(await pick.textContent(), l.auto);
        // About is long: scrolled to by the wheel, as a reader does (a
        // scroll Playwright makes isn't the reader's, and a save's redraw
        // puts the view back where the reader left it)
        await page.locator("#view-settings").hover();
        for (let i = 0; i < 40; i++) {
          const box = await row.boundingBox(), v = await page.locator("#view-settings").boundingBox();
          if (box.y >= v.y + 4 && box.y + box.height <= v.y + v.height - 4) break;
          await page.mouse.wheel(0, 180);
          await page.waitForTimeout(16);
        }
        const choose = async (name, want) => {
          const count = posts.length;
          const response = page.waitForResponse((r) => r.url().endsWith("/api/settings") && r.request().method() === "POST");
          await pick.click();
          await page.locator(".proto-menu").waitFor();
          assert.equal(await page.locator(".proto-menu .pm-item").count(), 3);
          await page.locator(".proto-menu").getByRole("menuitemradio", { name, exact: true }).click();
          await response;
          await page.waitForFunction((word) => document.querySelector("#status")?.textContent.includes(word), l.saved);
          assert.equal(posts.length, count + 1, "save once");
          assert.equal(posts.at(-1).updateSource, want);
          assert.equal(posts.at(-1).updateProxy, "", "the update proxy stays as it was");
          assert.equal(posts.at(-1).proxy, "direct", "the global proxy stays as it was");
          await page.waitForFunction((word) => document.querySelector("#updateSourcePick")?.textContent === word, name);
        };
        // the button is as wide as its name (jiangshangmuyun on X, Windows):
        // a percentage width inside the row's value box shrank it to a
        // clipped glyph and the chevron
        const fits = async (name) => {
          const m = await pick.evaluate((b) => {
            const s = b.querySelector("span");
            return { span: s.getBoundingClientRect().width, text: s.scrollWidth, pick: b.getBoundingClientRect().width };
          });
          assert(m.text <= Math.ceil(m.span) + 1, `${name}: the label is clipped (${m.text}px in ${m.span}px, button ${m.pick}px)`);
          assert(m.span > 0, `${name}: the label shows`);
        };
        // Download source above says what the source picked does: site never
        // asks GitHub (so no mirror), GitHub only never usemagpie.ai
        const says = async (key, host) => {
          const got = await page.evaluate(() => ({ sub: document.querySelector(".update-mirror-row .who .sub")?.firstChild?.textContent, code: document.querySelector(".update-mirror-row code")?.textContent }));
          assert.equal(got.sub, await page.evaluate((k) => t(k), key));
          assert.equal(got.code, host);
        };
        await says("Updates are downloaded from usemagpie.ai, and from GitHub when that fails; a mirror can stand in for GitHub where it is slow", "usemagpie.ai");
        await fits(l.auto);
        await choose(l.github, "github");
        await fits(l.github);
        await says("Update source is GitHub only: updates come from GitHub; a mirror can stand in for it where it is slow", "github.com");
        await choose(l.site, "site");
        await fits(l.site);
        await says("Update source is usemagpie.ai only: updates never come from GitHub, so no mirror is used", "usemagpie.ai");
        await choose(l.auto, "");
        await says("Updates are downloaded from usemagpie.ai, and from GitHub when that fails; a mirror can stand in for GitHub where it is slow", "usemagpie.ai");
        // the menu ticks the one chosen
        await pick.click();
        assert.equal(await page.locator(".proto-menu .pm-item.on").textContent(), l.auto);
        await page.keyboard.press("Escape");
        await openPart(page, "about");
        await page.locator("#about .update-mirror-row").waitFor();
        assert.equal(await page.locator("#updateSigningRow").count(), 0, "unsupported platforms have no local signing row");
        const over = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
        assert(over <= 0, `the page scrolls sideways by ${over}px`);
        assert.deepEqual(errors, []);
      });
      test(`${engine} ${lang} ${width}px: local signing in About retains the exact certificate and failed saves`, async (t) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        t.after(() => browser.close());
        const context = await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce", serviceWorkers: "block" });
        const page = await context.newPage();
        page.setDefaultTimeout(8000);
        const posts = [], errors = [];
        const signing = { supported: true, reads: [] };
        page.on("pageerror", (e) => errors.push(e.message));
        await context.route("**/*", server(lang, posts, signing));
        await watchSettingsReads(page);
        await page.goto("http://magpie.test/?view=settings&tab=network");
        await settingsDrawn(page);
        const row = page.locator("#updateSigningRow"), pick = page.locator("#updateSigningPick");
        await pick.waitFor({ state: "attached" });
        assert.equal(await row.evaluate((element) => element.closest(".set-page").id), "setPage-about", "local signing belongs to Settings About");
        assert.equal(await page.locator("#setPage-network #updateSigningRow").count(), 0);
        await openPart(page, "about");
        const search = page.locator("#setQ");
        await search.fill(await row.locator(".name").textContent());
        await row.waitFor();
        await search.press("Escape");
        const sourcePick = page.locator("#updateSourcePick");
        await reach(page, sourcePick);
        await sourcePick.click();
        const sourceMenu = page.getByRole("menu");
        await sourceMenu.getByRole("menuitemradio", { name: l.auto, exact: true }).waitFor();
        assert.equal(await sourceMenu.getByRole("menuitemradio").count(), 3, "About retains the update source menu");
        await page.keyboard.press("Escape");
        await pick.waitFor();
        assert.equal(await row.locator("select").count(), 0);
        assert(await row.evaluate((element) => element.previousElementSibling.matches(".update-mirror-row")), "signing follows Download source");
        assert.equal(await page.locator("#about .update-auto-row").count(), 1);
        assert.equal(await page.locator("#about .update-download-row").count(), 1);
        const off = await pick.textContent();
        assert.equal(posts.length, 0, "rendering never enrolls a certificate");
        assert(signing.reads.every((read) => read === "GET"), "rendering does not discover or select certificates");
        const openMenu = async () => {
          await reach(page, pick);
          await pick.click();
          const menu = page.locator(".proto-menu");
          await menu.waitFor();
          return menu;
        };
        const checkPin = async (fingerprint) => {
          const label = fingerprint ? fingerprint.slice(-8) : off;
          await page.waitForFunction((value) => document.querySelector("#updateSigningPick")?.textContent === value, label);
          if (fingerprint) assert.equal(await pick.getAttribute("title"), `${fingerprint} · TEAM · https://fork.test/latest.json`);
          const menu = await openMenu();
          const selected = menu.locator('[aria-checked="true"]');
          assert.equal(await selected.count(), 1);
          if (fingerprint) assert.equal(await selected.locator(".pm-note").textContent(), `TEAM · ${fingerprint}`);
          else assert.equal(await selected.locator(".pm-name").textContent(), off);
          await page.keyboard.press("Escape");
        };
        const choose = async (fingerprint, failure = "") => {
          const count = posts.length;
          const menu = await openMenu();
          assert.equal(await menu.getByText("Same signing name · AAAAAAAA", { exact: true }).count(), 1);
          assert.equal(await menu.getByText("Same signing name · BBBBBBBB", { exact: true }).count(), 1);
          const response = page.waitForResponse((reply) => reply.url().endsWith("/api/update/signing") && reply.request().method() === "POST");
          const option = fingerprint
            ? menu.getByRole("menuitemradio").filter({ has: page.getByText(`TEAM · ${fingerprint}`, { exact: true }) })
            : menu.getByRole("menuitemradio").first();
          await option.click();
          await response;
          await page.waitForFunction(({ word, failure }) => {
            const status = document.querySelector("#status");
            return !document.querySelector("#updateSigningPick")?.disabled && status?.textContent.includes(word) && (!failure || status.classList.contains("err"));
          }, { word: failure || l.saved, failure: !!failure });
          if (failure) {
            const alert = page.locator('#status[role="alert"]');
            assert.equal(await alert.innerText(), failure);
            assert(await alert.isVisible(), "the signing failure is visible");
          }
          assert.equal(posts.length, count + 1, "each explicit choice writes only once");
          assert.deepEqual(posts.at(-1), { signing: fingerprint ? { fingerprint } : { off: true } });
        };
        const redraw = async () => {
          const oldRow = await row.elementHandle();
          await page.evaluate(() => renderSettings());
          assert.equal(await oldRow.evaluate((element) => element.isConnected), false, "Settings really redraws the row");
          await pick.waitFor();
          assert.equal(await row.evaluate((element) => element.closest(".set-page").id), "setPage-about");
          assert.equal(await row.count(), 1);
        };
        for (const fingerprint of ["B".repeat(40), "A".repeat(40)]) {
          await choose(fingerprint);
          await checkPin(fingerprint);
          const writes = posts.length;
          await redraw();
          await checkPin(fingerprint);
          await page.reload();
          await settingsDrawn(page);
          await pick.waitFor();
          await checkPin(fingerprint);
          assert.equal(posts.length, writes, "redraw and reload do not enroll a certificate");
        }
        signing.listError = "Keychain identities unavailable";
        await pick.click();
        await page.waitForFunction((word) => document.querySelector("#status.err")?.textContent.includes(word), signing.listError);
        assert(await page.getByRole("alert").isVisible(), "the discovery failure is visible");
        assert.equal(await page.locator(".proto-menu").count(), 0);
        signing.listError = "";
        await checkPin("A".repeat(40));
        signing.saveError = "local signing does not support this app’s signature options";
        await choose("B".repeat(40), signing.saveError);
        await checkPin("A".repeat(40));
        await redraw();
        await checkPin("A".repeat(40));
        await page.reload();
        await settingsDrawn(page);
        await pick.waitFor();
        await checkPin("A".repeat(40));
        signing.saveError = "";
        await choose("B".repeat(40));
        await checkPin("B".repeat(40));
        signing.saveError = "local signing does not support this app’s signature options";
        await choose("", signing.saveError);
        await checkPin("B".repeat(40));
        signing.saveError = "";
        await choose("");
        await checkPin("");
        await redraw();
        await checkPin("");
        await page.reload();
        await settingsDrawn(page);
        await pick.waitFor();
        await checkPin("");
        assert.equal(posts.length, 6, "only explicit signing policy writes, including failed writes");
        assert(posts.every((body) => Object.keys(body).length === 1 && body.signing), "signing never writes synchronized Settings");
        assert(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth));
        assert.deepEqual(errors, []);
      });
    }
  }
}
