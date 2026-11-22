// Run with Node's test runner and Playwright on the module path; see README.md.
// Installed is narrowed as Discover is (WJaunita45604 on X: 要求在已安装的
// 插件里面，也要按照发现界面里面的分类…并且可以筛选是否为官方插件，具体实现
// 分类方法与发现界面里的分类方法一致): the same strip of kinds, each with how
// many are installed, and the same Origin menu (magpie community, other
// authors, unofficial on GitHub), with nothing about installing. A plugin
// Discover lists is put where its card is; one it doesn't (a folder, an
// npm package nobody listed) by what it is. A pick that leaves nothing
// says so with Show all, and Discover's own picks are left alone. Seven
// plugins: a community subscription and middleware, another author's
// agent, two repositories tagged on GitHub (a provider and a middleware,
// from the dev instance's real /api/plugins/github answer), a middleware
// in a folder and an unlisted npm subscription. The API is faked and no
// request leaves the page. Every language, both engines, 1100px and 420px.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const GH = require("./fixtures/plugins-github-31.json");
const LISTINGS = [
  { package: "@magpie-community/opencode-zed-auth", name: "Zed", community: true },
  { package: "@magpie-community/opencode-think-tags", name: "Think tags", community: true, kind: "middleware" },
  { package: "opencode-some-agent", name: "Some Agent", kind: "agent" },
];
const ghEntry = (repo) => { const r = GH.repos.find((x) => x.repo === repo); return { spec: r.spec, package: r.package, providers: [], version: "1.0.0", moved: [] }; };
const INSTALLED = [
  { spec: "@magpie-community/opencode-zed-auth", providers: [], version: "0.2.0", moved: [] },
  { spec: "@magpie-community/opencode-think-tags", providers: [], version: "0.1.2", moved: [], isMiddleware: true, middlewareOnly: true },
  { spec: "opencode-some-agent", providers: [], version: "1.0.0", moved: [], isAgent: true },
  ghEntry("mu-zi-lee/magpie-our-free-model"),
  ghEntry("ezdemo/magpie-haiku55-thinking"),
  { spec: "/Users/me/plugins/my-hook", ownName: "My hook", providers: [], moved: [], isMiddleware: true, middlewareOnly: true },
  { spec: "opencode-unlisted-auth", providers: [], version: "3.0.0", moved: [] },
];
// what Discover calls each: [kind, origin]
const WANT = {
  "Zed": ["pv", "ours"], "Think tags": ["mw", "ours"], "Some Agent": ["ag", "third"],
  "magpie-our-free-model": ["pv", "gh"], "magpie-opendesign-compatible-fix": ["mw", "gh"],
  "My hook": ["mw", "third"], "opencode-unlisted-auth": ["pv", "third"],
};

function server(lang, fetched, installed = INSTALLED) {
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true }, plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (url.pathname === "/api/gateway/trace") return json({ routes: [] });
    if (url.pathname === "/api/plugins") return json({ bun: true, bunVersion: "1.4.2", plugins: installed, picker: false, movable: [] });
    if (url.pathname === "/api/plugins/listings") return json({ listings: LISTINGS });
    if (url.pathname === "/api/plugins/npm") return json({ npm: {} });
    if (url.pathname === "/api/plugins/github") return json(GH);
    if (url.pathname.startsWith("/api/")) return json({});
    if (url.host !== "magpie.test" || !/^\/[\w./-]*$/.test(url.pathname)) { fetched.push(url.href); return route.fulfill({ status: 404, body: "" }); }
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { await route.fulfill({ body: await fs.readFile(file), contentType }); } catch { await route.fulfill({ status: 404, body: "" }); }
  };
}

const names = (k, src) => Object.keys(WANT).filter((n) => (!k || WANT[n][0] === k) && (!src || WANT[n][1] === src)).sort();

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": Installed is narrowed by Discover's kinds and origins", async (t) => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) for (const width of [1100, 420]) {
      await t.test(`${lang} ${width}px`, async () => {
        const ctx = await browser.newContext({ viewport: { width, height: 860 } });
        const page = await ctx.newPage();
        page.setDefaultTimeout(6000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [], fetched = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", server(lang, fetched));
        try {
          await page.goto("http://magpie.test/?view=plugins");
          const view = page.locator("#view-plugins");
          await view.locator(".pm-bar").first().waitFor();
          await view.locator(".pm-ghlook").waitFor({ state: "detached" });
          const tr = (s) => page.evaluate((s) => t(s), s);
          // the tabs are in the sidebar's list on some widths
          const tab = async (s) => page.locator(":is(.lib-tabs .opt, .nav-s):visible", { hasText: await tr(s) }).first().click();
          await tab("Installed");
          const head = view.locator(".pm-listhead");
          await head.locator(".pm-kinds").waitFor();
          const rows = view.locator(".pm-list .pm-row");
          const shown = async () => (await rows.locator(".who .name > span:first-child").allInnerTexts()).map((s) => s.trim()).sort();
          const kinds = head.locator(".pm-kinds .opt");
          const said = () => kinds.evaluateAll((bs) => bs.map((b) => [b.firstChild.textContent, b.querySelector("em").textContent]));
          const want = async (src) => Promise.all([["", "All"], ["pv", "Subscriptions"], ["mw", "Middleware"], ["ag", "Agents"]].map(async ([k, n]) => [await tr(n), String(names(k, src).length)]));
          const pop = head.locator(".pm-pop");
          const choose = async (name) => {
            await pop.click();
            await page.locator(".proto-menu .pm-item", { hasText: await tr(name) }).first().click();
            await page.locator(".proto-menu").waitFor({ state: "hidden" }).catch(() => {});
          };

          // every one, and the strip counts each kind as Discover would
          assert.deepEqual(await shown(), names());
          assert.deepEqual(await said(), await want());
          assert.equal(await view.locator("select").count(), 0, "no native select");
          const overflow = () => page.evaluate(() => {
            const h = document.querySelector("#view-plugins .pm-listhead"), v = document.querySelector("#view-plugins");
            return [h.scrollWidth - h.clientWidth, v.scrollWidth - v.clientWidth, document.documentElement.scrollWidth - innerWidth].map((x) => Math.max(0, x));
          });
          assert.deepEqual(await overflow(), [0, 0, 0]);

          // a kind
          await kinds.nth(2).click();
          assert.deepEqual(await shown(), names("mw"));
          // and an origin: the strip counts what it leaves
          await pop.click();
          assert(!(await page.locator(".proto-menu .pm-item", { hasText: await tr("Not installed only") }).count()), "no Not installed only");
          await page.keyboard.press("Escape");
          await page.locator(".proto-menu").waitFor({ state: "hidden" }).catch(() => {});
          await choose("magpie community");
          assert.deepEqual(await shown(), names("mw", "ours"));
          assert.deepEqual(await said(), await want("ours"));
          assert.equal((await pop.locator("b").textContent()).trim(), await tr("magpie community"));
          await choose("Unofficial, on GitHub");
          assert.deepEqual(await shown(), names("mw", "gh"));
          await kinds.nth(0).click();
          assert.deepEqual(await shown(), names("", "gh"));
          await choose("Other authors");
          assert.deepEqual(await shown(), names("", "third"));
          assert.deepEqual(await said(), await want("third"));

          // a pick that leaves nothing: said, and Show all puts back all
          await choose("Unofficial, on GitHub");
          await kinds.nth(3).click();
          assert.equal(await rows.count(), 0);
          const none = view.locator(".pm-ghnone");
          assert.equal((await none.locator("span").textContent()).trim(), await tr("No plugin matches"));
          await none.locator("button").click();
          assert.deepEqual(await shown(), names());
          assert.equal(await head.locator(".pm-kinds .opt.on").evaluate((b) => b.firstChild.textContent), await tr("All"));
          assert.equal((await pop.locator("b").textContent()).trim(), await tr("All"));

          // Discover's own picks are its own
          await kinds.nth(1).click();
          await tab("Discover");
          const dbar = view.locator(".pm-body > .pm-bar");
          await dbar.waitFor();
          assert.equal(await dbar.locator(".pm-kinds .opt.on").evaluate((b) => b.firstChild.textContent), await tr("All"));
          // and back, the pick kept
          await tab("Installed");
          await head.locator(".pm-kinds").waitFor();
          assert.deepEqual(await shown(), names("pv"));
          assert.deepEqual(await overflow(), [0, 0, 0]);
          assert.deepEqual(errors, []);
          assert.deepEqual(fetched, []);
        } finally {
          await ctx.close();
        }
      });
    }
  });

  test(engine + ": Installed filters SDK pages and session contributions", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
    t.after(() => browser.close());
    const sdk = { runtime: "native", api: "1.18", capabilities: [] };
    const base = { providers: [], moved: [], version: "1.0.0", inMagpieOnly: true };
    const installed = [
      { ...base, spec: "sdk:panel", ownName: "Panel only", providers: ["Fixture provider"], sdk: { ...sdk, window: true },
        panels: [{ slot: "sessions", title: "History controls" }] },
      { ...base, spec: "sdk:page", ownName: "Standalone SDK", sdk: { ...sdk, window: true },
        page: { title: "Standalone page", reads: [] } },
      { ...base, spec: "sdk:omp-sessions", ownName: "OMP Sessions", sdk: { ...sdk, sessions: ["omp-export", "omp-storage"] } },
      { ...base, spec: "legacy-page", ownName: "Legacy standalone", page: { title: "Legacy page", reads: [] } },
    ];
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      const page = await browser.newPage({ viewport: { width: 420, height: 850 } });
      const fetched = [], errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", server(lang, fetched, installed));
      try {
        await page.goto("http://magpie.test/?view=plugins");
        const tr = (value) => page.evaluate((value) => t(value), value);
        await page.locator(":is(.lib-tabs .opt, .nav-s):visible", { hasText: await tr("Installed") }).first().click();
        const view = page.locator("#view-plugins");
        const kinds = view.locator(".pm-listhead .pm-kinds .opt");
        for (const [label, want] of [["Pages", ["Legacy standalone", "Standalone SDK"]], ["Sessions", ["OMP Sessions"]]]) {
          await t.test(`${lang} ${label}`, async () => {
            const pick = kinds.filter({ hasText: await tr(label) });
            await pick.click({ timeout: 4000 });
            const rows = view.locator(".pm-list .pm-row .who .name > span:first-child");
            assert.deepEqual((await rows.allInnerTexts()).map((value) => value.trim()).sort(), want);
            assert.equal(await pick.locator("em").textContent(), String(want.length));
          });
        }
        assert.deepEqual(errors, []);
        assert.deepEqual(fetched, []);
      } finally {
        await page.close();
      }
    }
  });

  test(engine + ": SDK release uses the ordinary plugin controls", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: process.env.PLAYWRIGHT_CHANNEL || "chromium" }));
    t.after(() => browser.close());
    const page = await browser.newPage({ viewport: { width: 420, height: 850 } });
    page.setDefaultTimeout(8000);
    const source = "github:TransmuteLabs/magpie-sdk-community-plugins#opencode";
    const spec = "sdk:opencode";
    const fixture = {
      spec, ownName: "OpenCode", providers: ["OpenCode Go"], version: "0.8.0", moved: [], isAgent: true,
      sdk: { source, runtime: "native", api: "1.15", hostAPI: "^1", checkable: true, window: true,
        description: "OpenCode provider and agent", agents: ["OpenCode SDK"], capabilities: ["agent-wiring", "outbound-executor"],
        platforms: ["darwin/arm64"], nativeABI: 1, protocols: ["chat"], sessions: ["opencode-history"] },
    };
    let installed;
    const apiCalls = [], npmNames = [], fetched = [], errors = [];
    const base = server("en", fetched);
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", (route) => {
      const url = new URL(route.request().url()), tail = url.pathname.replace(/^\/api\/plugins\/?/, "");
      const state = () => ({ bun: false, plugins: [INSTALLED[0], installed].filter(Boolean), picker: false, movable: [] });
      if (url.pathname === "/api/plugins") return route.fulfill({ json: state() });
      if (tail === "npm") npmNames.push(...String(url.searchParams.get("names") || "").split(","));
      if (!url.pathname.startsWith("/api/plugins/") || route.request().method() !== "POST") return base(route);
      const body = route.request().postDataJSON();
      apiCalls.push([tail, body]);
      switch (tail) {
        case "add": installed = structuredClone(fixture); break;
        case "check":
          installed.latest = "0.9.0";
          installed.sdk.check = { spec, package: "opencode", version: installed.version, status: "update", latest: installed.latest };
          return route.fulfill({ json: { at: new Date().toISOString(), plugins: [installed.sdk.check], state: state() } });
        case "upgrade":
          installed.version = installed.latest;
          delete installed.latest;
          delete installed.sdk.check;
          break;
        case "off": installed.off = body.off; break;
        case "remove": installed = null; break;
        default: return base(route);
      }
      return route.fulfill({ json: state() });
    });

    await page.goto("http://magpie.test/?view=plugins");
    const view = page.locator("#view-plugins");
    const manual = view.locator(".pm-manual");
    await manual.getByRole("textbox").fill(source);
    await manual.getByRole("button", { name: "Install", exact: true }).click();
    await manual.getByRole("button", { name: "Install", exact: true }).waitFor();
    await page.locator(":is(.lib-tabs .opt, .nav-s):visible", { hasText: "Installed" }).first().click();
    const row = view.locator(".pm-row", { has: page.getByText("OpenCode", { exact: true }) });
    await row.getByText("v0.8.0", { exact: true }).waitFor();
    await row.locator("summary").click();
    await row.getByText("OpenCode provider and agent", { exact: true }).waitFor();
    const details = await row.locator("details").innerText();
    for (const value of ["OpenCode Go", "OpenCode SDK", "opencode-history", "chat", "darwin/arm64", source]) assert(details.includes(value), value);
    for (const label of ["Subscriptions", "Agents"]) {
      await view.locator(".pm-listhead .pm-kinds .opt", { hasText: label }).click();
      assert.equal(await row.count(), 1, "two-capability module belongs under " + label);
    }
    await view.getByRole("button", { name: "Check for updates" }).click();
    await row.getByRole("button", { name: "Update", exact: true }).click();
    await row.getByText("v0.9.0", { exact: true }).waitFor();
    await row.getByRole("button", { name: "Switch off" }).click();
    await row.getByRole("button", { name: "Switch on" }).click();
    await row.getByRole("button", { name: "Switch off" }).waitFor();
    await row.getByRole("button", { name: "Remove", exact: true }).click();
    await page.getByRole("alertdialog").getByRole("button", { name: "Remove", exact: true }).click();
    await row.waitFor({ state: "detached" });
    assert.deepEqual(apiCalls, [
      ["add", { spec: source }], ["check", {}], ["upgrade", { spec }], ["off", { spec, off: true }],
      ["off", { spec, off: false }], ["remove", { spec }],
    ]);
    assert(!npmNames.includes("opencode") && !npmNames.includes(spec), "SDK release was sent to npm");
    assert.deepEqual(errors, []);
    assert.deepEqual(fetched, []);
  });
}
