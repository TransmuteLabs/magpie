const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(process.env.MAGPIE_ASSETS || path.resolve(__dirname, "../assets"));

async function serve(route) {
  const url = new URL(route.request().url());
  assert.equal(url.origin, "http://magpie.test");
  const json = (data) => route.fulfill({ json: data });
  if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:false,sideGateway:{url:"http://127.0.0.1:3425",running:true,boot:true}};' });
  if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
  if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang: "en", theme: "light" } });
  if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { url: "http://127.0.0.1:3425", running: true, window: true } });
  if (url.pathname === "/api/groups") return json({ groups: [] });
  if (url.pathname.startsWith("/api/")) return json({});
  const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
  const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
  let body = await fs.readFile(file);
  if (url.pathname === "/app.css") body = Buffer.from(body.toString().replace(/^.*:has\([^\n]*$/gm, ""));
  return route.fulfill({ body, contentType });
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const reduced of [false, true]) {
    test(`${engine}: without :has, Settings keeps its footer space through ${reduced ? "reduced" : "animated"} close and reopen`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await browser.newPage({ viewport: { width: 1200, height: 340 }, reducedMotion: reduced ? "reduce" : "no-preference" });
      page.setDefaultTimeout(8000);
      page.setDefaultNavigationTimeout(60000);
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.addInitScript(() => { delete Element.prototype.checkVisibility; });
      await page.route("**/*", serve);
      await page.goto("http://magpie.test/?view=agents");
      await page.locator("#nav button.on").waitFor();
      assert.equal(await page.evaluate(() => document.body.classList.contains("side-nav")), true);
      const foot = page.locator(".side-foot");
      assert.equal(await foot.evaluate((node) => getComputedStyle(node).maxHeight), "55%");
      await page.evaluate(() => {
        window.footerSamples = [];
        window.footerSampling = true;
        const sample = () => {
          const foot = document.querySelector(".side-foot");
          window.footerSamples.push({ tabs: !!foot.querySelector(":scope > .nav-sub"), maxHeight: getComputedStyle(foot).maxHeight });
          if (window.footerSampling) requestAnimationFrame(sample);
        };
        sample();
      });
      const prefs = page.locator("#prefs");
      const before = await prefs.boundingBox();
      await prefs.click();
      await page.locator(".side-foot .nav-s").first().waitFor();
      assert.equal(await page.locator(".side-foot .nav-s").count(), 8);
      assert.equal(await foot.evaluate((node) => getComputedStyle(node).maxHeight), "80%", "the Settings list has the original larger allowance without :has");
      await page.waitForFunction(() => !document.querySelector(".side-foot .nav-sub.moving"));
      const after = await prefs.boundingBox();
      assert.ok(Math.abs(after.y - before.y) < 1, "the Settings button stays under the pointer");
      assert.equal(await foot.evaluate((node) => node.scrollHeight > node.clientHeight + 1), true, "the original tiny-window list scrolls");
      const overlapping = await page.evaluate(() => {
        document.querySelector('#nav button[data-view="agents"]').click();
        document.querySelector("#prefs").click();
        return document.querySelectorAll(".side-foot > .nav-sub").length;
      });
      assert.equal(overlapping, reduced ? 1 : 2, "a reopening preserves both the leaving list and its successor until the animation joins");
      await page.waitForFunction(() => !document.querySelector(".side-foot .nav-sub.leaving"));
      assert.equal(await foot.evaluate((node) => getComputedStyle(node).maxHeight), "80%", "the old list finishing cannot shrink a reopened list");
      await page.locator('#nav button[data-view="agents"]').click();
      await page.waitForFunction(() => !document.querySelector(".side-foot > .nav-sub"));
      assert.equal(await foot.evaluate((node) => getComputedStyle(node).maxHeight), "55%", "the ordinary allowance returns after the last list leaves");
      await page.evaluate(() => { window.footerSampling = false; });
      const samples = await page.evaluate(() => window.footerSamples);
      assert.ok(samples.some((sample) => sample.tabs) && samples.some((sample) => !sample.tabs));
      assert.deepEqual(samples.filter((sample) => sample.maxHeight !== (sample.tabs ? "80%" : "55%")), [], "each rendered frame has the allowance belonging to its actual child list");
      assert.deepEqual(errors, []);
    });
  }
}
