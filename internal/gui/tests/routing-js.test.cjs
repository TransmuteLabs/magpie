// Run with Node's test runner and Playwright on the module path; see README.md.
// A JS gateway plugin's strategies are routings "<plugin>/<name>"
// (internal/middleware/points.go). The ones the gateway serves now
// (routingOptions in each ordinary state read) are offered in a provider's editor
// beside the built-in ones, say which plugin routes, and are saved as
// picked. A routing saved whose plugin serves it no more shows as it is,
// saying its accounts go in their listed order. Installed SDK strategies
// are independent of JS gateway middleware.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_ROUTING_ASSETS || path.resolve(__dirname, "../assets");
const legacy = { id: "spread/fewest-tokens", name: "fewest-tokens", plugin: "spread" };
const installed = { id: "sdk-spread/reverse", name: "Reverse", plugin: "sdk-spread" };
const builtins = ["", "order", "drain", "rotate", "usage", "pace", "weight", "free", "fast"].map((id) => ({ id }));
const base = { icon: "generic", catalog: "", models: [{ id: "glm-5", name: "GLM-5", on: true }], agents: [], fallback: [], headers: {}, routing: "", affinity: "", ready: true };
const sub = {
  ...base, id: "workbuddy-ai", name: "WorkBuddy AI", chat: "", responses: "", anthropic: "", keyList: [],
  account: {
    agent: "workbuddy-ai", agentName: "WorkBuddy AI", user: "one@example.test", plan: "Pro",
    logins: ["one", "two", "three"].map((n, i) => ({ user: `${n}@example.test`, plan: "Pro", active: i === 0, on: true })),
  },
};
const relay = {
  ...base, routing: "gone/old", id: "relay", name: "Relay", chat: "https://relay.example.invalid/v1", responses: "", anthropic: "", key: { set: true, masked: "sk-…1234" },
  keyList: [1, 2].map((i) => ({ id: `key-${i}`, name: `Key ${i}`, masked: `sk-…000${i}`, active: i === 1, on: true })),
};

function serve(lang, posts, off = [], active = () => [legacy], stateReply = (json, data) => json(data)) {
  const settings = { lang, theme: "light" };
  const providers = { providers: [sub, relay], presets: [], excluded: [], gateway: { running: true, window: true } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:${JSON.stringify(lang)},theme:"light",web:true,featuresOff:${JSON.stringify(off)}};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    const routingOptions = [...builtins, ...active()];
    if (url.pathname === "/api/state") return stateReply(json, { agents: [], profiles: [], settings, routingOptions });
    // what the real one answers: the state's settings (a provider's editor
    // reads them for its own warm-ups and check-ins)
    if (url.pathname === "/api/settings" && route.request().method() === "GET") return json({ ...settings, jsRoutings: active(), routingOptions });
    if (url.pathname === "/api/providers") return json({ ...providers, routingOptions });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (route.request().method() === "POST" && url.pathname.startsWith("/api/provider")) {
      posts.push({ path: url.pathname, body: route.request().postDataJSON() });
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    await route.fulfill({ body: await fs.readFile(file), contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)] });
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh"]) {
    const zh = lang === "zh";
    const L = { routing: zh ? "路由" : "Routing", save: zh ? "保存" : "Save" };
    test(`${engine} ${lang}: a JS plugin's routings are offered, said and saved; one no plugin serves shows as it is`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
      t.after(() => browser.close());
      for (const width of [900, 420]) {
        const page = await (await browser.newContext({ viewport: { width, height: 720 }, reducedMotion: "reduce" })).newPage();
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [], posts = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang, posts));
        await page.goto("http://magpie.test/?view=providers");
        const missing = await page.evaluate(() => [
          "Routed by {name}, a routing of the gateway plugin {plugin}: it orders the accounts or keys for each request. If it fails or takes too long, they are tried in their listed order.",
          "No plugin serves this routing now, so requests go to the accounts or keys in their listed order. Turn on the gateway plugin that serves it on the Plugins page.",
        ].filter((k) => ["zh", "zh-TW", "ja", "de"].some((l) => !I18N[l][k])));
        assert.deepEqual(missing, [], "every string has its translations");
        const field = () => page.locator(".editor label", { hasText: new RegExp(`^${L.routing}$`) }).locator("xpath=following-sibling::div[1]");
        const shown = () => field().locator(".segs").first().evaluate((box) => [...box.querySelectorAll(".opt")].filter((b) => getComputedStyle(b).display !== "none").map((b) => [b.textContent, b.classList.contains("on")]));
        const hint = () => field().locator(":scope > .hint").first().textContent();
        for (const p of [sub, relay]) {
          const where = `${width}px ${p.name}`;
          await page.locator(".row.provider", { hasText: p.name }).first().click();
          await page.locator(".editor .accts").first().waitFor();
          // Routing is under Advanced, open already when one is set (Relay's)
          const adv = page.locator(".editor details.advanced");
          assert.equal(await adv.evaluate((d) => d.open), p === relay, `${where}: Advanced open`);
          if (p !== relay) await adv.locator(":scope > summary").click();
          const opts = await shown();
          assert.ok(opts.some(([n]) => n === "spread/fewest-tokens"), `${where}: offered ${JSON.stringify(opts)}`);
          if (p === relay) {
            // saved, served by nothing: shown as it is, and picked
            assert.deepEqual(opts.at(-1), ["gone/old", true], `${where}: the saved routing`);
            assert.equal(await hint(), await page.evaluate(() => t("No plugin serves this routing now, so requests go to the accounts or keys in their listed order. Turn on the gateway plugin that serves it on the Plugins page.")), where);
          } else assert.ok(!opts.some(([n]) => n === "gone/old"), `${where}: not another provider's`);
          await field().locator(".segs .opt", { hasText: /^spread\/fewest-tokens$/ }).click();
          assert.equal(await hint(), await page.evaluate(() => t("Routed by {name}, a routing of the gateway plugin {plugin}: it orders the accounts or keys for each request. If it fails or takes too long, they are tried in their listed order.", { name: "fewest-tokens", plugin: "spread" })), where);
          assert.ok(await page.evaluate(() => document.scrollingElement.scrollWidth <= innerWidth + 1), `${where}: fits`);
          await page.getByRole("button", { name: L.save, exact: true }).click();
          await page.waitForFunction(() => !document.querySelector(".editor"));
          assert.equal(posts.at(-1).body.routing, "spread/fewest-tokens", `${where}: saved`);
        }
        assert.deepEqual(errors, []);
        await page.context().close();
      }
    });
    test(`${engine} ${lang}: SDK routing survives middleware off and refreshes on the ordinary state read`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
      t.after(() => browser.close());
      const page = await (await browser.newContext({ viewport: { width: 900, height: 720 }, reducedMotion: "reduce" })).newPage();
      page.setDefaultTimeout(5000);
      page.setDefaultNavigationTimeout(60000);
      let active = [installed];
      let waitForState, omitCatalog;
      const entered = Promise.withResolvers();
      const release = Promise.withResolvers();
      t.after(() => release.resolve());
      await page.route("**/*", serve(lang, [], ["middleware"], () => active, async (json, data) => {
        if (waitForState) {
          waitForState = false;
          entered.resolve();
          await release.promise;
        }
        if (omitCatalog) delete data.routingOptions;
        return json(data);
      }));
      await page.goto("http://magpie.test/?view=providers");
      const field = page.locator(".editor label", { hasText: new RegExp(`^${L.routing}$`) }).locator("xpath=following-sibling::div[1]");
      const names = () => field.locator(".segs").first().evaluate((box) => [...box.querySelectorAll(".opt")].filter((b) => getComputedStyle(b).display !== "none").map((b) => b.textContent));
      const open = async () => {
        await page.locator(".row.provider", { hasText: relay.name }).first().click();
        await page.locator(".editor .accts").first().waitFor();
      };
      const close = () => page.locator(".editor .ed-cancel").click();
      await open();
      assert.ok((await names()).includes(installed.id), "the installed strategy is visible with middleware off");
      assert.ok(!(await names()).includes(legacy.id), "the inactive legacy strategy is absent");
      await close();
      omitCatalog = true;
      await page.evaluate(() => api("state"));
      omitCatalog = false;
      await open();
      assert.ok((await names()).includes(installed.id), "a read without catalog data leaves the previous catalog");
      await close();
      waitForState = true;
      const older = page.evaluate(() => api("state"));
      await entered.promise;
      active = [];
      await page.evaluate(() => load(true));
      release.resolve();
      await older;
      await open();
      assert.ok(!(await names()).includes(installed.id), "a late state read cannot bring back a disabled strategy");
      assert.equal((await names()).at(-1), "gone/old", "the stored strategy remains visible");
      await close();
      active = [installed];
      await page.evaluate(() => api("state?last=1"));
      await open();
      assert.ok(!(await names()).includes(installed.id), "the startup snapshot cannot replace a live catalog");
    });
  }
}
