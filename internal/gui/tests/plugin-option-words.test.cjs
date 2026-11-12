// Run with Node's test runner and Playwright on the module path; see README.md.
// A plugin option in the reader's language, and a short model list said to
// be filtered (plugins#590, anewuser6937 on Discord): WorkBuddy's "Free
// models only" showed in English inside the Chinese window, and with it on
// the editor listed one model with nothing saying why, so the plugin read as
// broken. The option's labels, descriptions and confirms give its words by
// the window's language (zh-TW falls back to zh, any other to the plain
// one), and while a switch that says filtersModels is on, as saved, the
// Models field says so under the chips. Off, nothing is said. Both engines,
// 1100 and 420 wide, every language.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { pathToFileURL } = require("node:url");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");

const free = (on) => ({
  key: "free_only", label: "Free models only", description: "Lists only the models WorkBuddy serves at no cost.",
  labels: { zh: "仅免费模型", "zh-TW": "僅免費模型", ja: "無料モデルのみ", de: "Nur kostenlose Modelle" },
  descriptions: { zh: "只列出 WorkBuddy 免费提供的模型。", "zh-TW": "只列出 WorkBuddy 免費提供的模型。", ja: "WorkBuddy が無料で提供するモデルだけを表示します。", de: "Listet nur die Modelle, die WorkBuddy kostenlos anbietet." },
  filtersModels: true, on,
});
// only Simplified Chinese words: zh-TW reads them, the rest the plain ones
const beta = { key: "beta", label: "Beta models", confirm: "Beta models may be withdrawn.", labels: { zh: "测试版模型" }, confirms: { zh: "测试版模型可能会下线。" }, on: false };

function serve(lang, on) {
  const plugin = { id: "workbuddy", pid: "workbuddy", name: "WorkBuddy", icon: "generic", spec: "@magpie-community/opencode-workbuddy-auth", signedIn: true, models: 1,
    methods: [{ type: "api", label: "API key" }], options: [free(on), beta] };
  const payload = { providers: [{ id: "workbuddy", name: "WorkBuddy", icon: "generic", chat: "plugin://workbuddy/v1", responses: "", anthropic: "", catalog: "",
    models: [{ id: "hy3", name: "Hy3", on: true }], agents: [], fallback: [], headers: {}, keyList: [], key: {}, proxy: "", maxConcurrency: null,
    account: { agent: "workbuddy", agentName: "WorkBuddy", agentIcon: "generic", user: "me@fake", logins: [{ user: "me@fake", active: true, on: true }] } }],
  // moved onto its plugin, as every WorkBuddy user is since 10-17
  presets: [], excluded: [], gateway: { running: true, window: true }, plugins: [plugin], onPlugins: ["workbuddy"] };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json(payload);
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { await route.fulfill({ body: await fs.readFile(file), contentType }); } catch { await route.fulfill({ status: 404, body: "" }); }
  };
}

const W = {
  en: { models: "Models", on: "On", label: "Free models only", about: "Lists only the models WorkBuddy serves at no cost.", beta: "Beta models", ask: "Beta models may be withdrawn.",
    note: "Only some models are listed: the plugin option “Free models only” is on." },
  zh: { models: "模型", on: "开启", label: "仅免费模型", about: "只列出 WorkBuddy 免费提供的模型。", beta: "测试版模型", ask: "测试版模型可能会下线。",
    note: "只列出部分模型：插件选项「仅免费模型」已开启。" },
  "zh-TW": { models: "模型", on: "開啟", label: "僅免費模型", about: "只列出 WorkBuddy 免費提供的模型。", beta: "测试版模型", ask: "测试版模型可能会下线。",
    note: "只列出部分模型：外掛選項「僅免費模型」已開啟。" },
  ja: { models: "モデル", on: "オン", label: "無料モデルのみ", about: "WorkBuddy が無料で提供するモデルだけを表示します。", beta: "Beta models", ask: "Beta models may be withdrawn.",
    note: "一部のモデルのみ表示しています。プラグインのオプション「無料モデルのみ」がオンです。" },
  de: { models: "Modelle", on: "An", label: "Nur kostenlose Modelle", about: "Listet nur die Modelle, die WorkBuddy kostenlos anbietet.", beta: "Beta models", ask: "Beta models may be withdrawn.",
    note: "Nur einige Modelle werden angezeigt: Die Plugin-Option „Nur kostenlose Modelle“ ist an." },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: ZCode's actual methods, prompts and options follow the window language`, { timeout: 60000 }, async (t) => {
    const moduleURL = pathToFileURL(path.resolve(__dirname, "../../plugin/bundled/zcode/index.mjs")).href;
    const { ZCodeAuthPlugin } = await import(moduleURL);
    const auth = (await ZCodeAuthPlugin()).auth;
    const methods = auth.methods.map(({ type, label, labels }) => ({ type, label, labels }));
    const options = auth.options.map((option) => ({ ...option, on: !!option.default }));
    const plugin = { id: "zcode", pid: "zcode", name: "ZCode", icon: "zcode", spec: "builtin:zcode", signedIn: true, methods, options };
    const provider = { id: "zcode", name: "ZCode", icon: "zcode", models: [], agents: [], fallback: [], headers: {}, keyList: [],
      account: { agent: "zcode", builtin: "zcode", agentName: "ZCode", user: "fixture", logins: [{ user: "fixture", active: true, on: true }] } };
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      await t.test(lang, async () => {
        const context = await browser.newContext({ viewport: { width: 960, height: 800 }, reducedMotion: "reduce" });
        const page = await context.newPage(), asked = [], errors = [];
        page.setDefaultTimeout(5000);
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", async (route) => {
          const request = route.request(), url = new URL(request.url());
          const json = (data) => route.fulfill({ json: data });
          if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:false};` });
          if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
          if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
          if (url.pathname === "/api/providers") return json({ providers: [provider], presets: [], excluded: [], onPlugins: [], plugins: [plugin], gateway: { running: true, window: true } });
          if (url.pathname === "/api/plugin-signin/prompt") {
            const body = request.postDataJSON(), inputs = { ...(body.inputs || {}) };
            asked.push(["prompt", body]);
            if (body.key) inputs[body.key] = body.value;
            return json({ inputs, prompt: inputs.site ? null : auth.methods[body.method].prompts?.[0] || null });
          }
          if (url.pathname === "/api/plugin-signin") {
            const input = request.postDataJSON();
            asked.push(["signin", input]);
            if (input.method === 6) return route.fulfill({ status: 400, json: { error: "fixture stops before authorization" } });
            return json({ agent: "zcode", state: "failed", error: "fixture stops before authorization" });
          }
          if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
          if (url.pathname.startsWith("/api/")) return json({});
          const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
          const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
          return route.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType });
        });
        try {
          await page.goto("http://magpie.test/?view=providers");
          await page.locator(".row.provider", { hasText: "ZCode" }).first().click();
          const ultra = page.locator('.plugin-option[data-key="coding_plan_ultra"]');
          await ultra.waitFor();
          const optionLabel = await ultra.locator(".n").textContent();
          const optionDescription = await ultra.locator(".hint").textContent();
          assert.ok(optionLabel && optionDescription);
          if (lang !== "en") {
            assert.notEqual(optionLabel, auth.options[1].label);
            assert.notEqual(optionDescription, auth.options[1].description);
          }
          for (const method of [4, 6]) {
            await page.goto("http://magpie.test/?view=providers");
            await page.waitForFunction(() => providers);
            await page.evaluate(() => {
              window.renderProviders = () => {
                const host = document.getElementById("zcode-language") || Object.assign(document.createElement("div"), { id: "zcode-language" });
                host.style.cssText = "position:fixed;inset:0 auto auto 0;width:100%;z-index:1000;";
                host.replaceChildren(signing ? renderSigning(subOf("zcode")) : "");
                document.body.append(host);
              };
              startSignIn("zcode", true);
            });
            const host = page.locator("#zcode-language");
            const way = host.locator(`[data-method="${method}"]`);
            await way.waitFor();
            if (lang !== "en") assert.notEqual(await way.textContent(), auth.methods[method].label);
            await way.click();
            const prompt = host.locator(".plugin-ask");
            await prompt.waitFor();
            const question = await prompt.locator(".tt .n").textContent();
            if (lang !== "en") assert.notEqual(question, auth.methods[method].prompts[0].message);
            const choices = prompt.locator(".choices button");
            assert.equal(await choices.count(), 2);
            if (lang !== "en") assert.notEqual(await choices.nth(1).textContent(), auth.methods[method].prompts[0].options[1].label);
            await choices.nth(1).click();
            if (method === 6) {
              const key = host.locator('.callback-form input[type="password"]');
              await key.waitFor();
              if (lang !== "en") assert.notEqual(await key.getAttribute("aria-label"), auth.methods[method].label);
              await key.fill("fixture-key");
              await key.press("Enter");
            }
            await page.waitForFunction(() => signing?.error?.includes("fixture stops before authorization"));
            const sent = asked.filter(([kind]) => kind === "signin").at(-1)?.[1];
            assert.equal(sent?.method, method);
            assert.equal(sent?.inputs?.site, "bigmodel");
          }
          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });
  test(engine + ": a plugin option's own words and its filtered list", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const width of [1100, 420]) for (const lang of Object.keys(W)) for (const on of [true, false]) {
      await t.test(`${width} ${lang} ${on ? "on" : "off"}`, async () => {
        const w = W[lang];
        const ctx = await browser.newContext({ viewport: { width, height: 800 }, reducedMotion: "reduce" });
        const page = await ctx.newPage();
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (e) => errors.push(e.message));
        await page.route("**/*", serve(lang, on));
        await page.goto("http://magpie.test/?view=providers");
        await page.locator(".row.provider", { hasText: "WorkBuddy" }).click();
        const ed = page.locator("#modal .editor");
        const box = ed.locator(".plugin-options");
        await box.waitFor();
        await page.waitForTimeout(300); // the editor drawn again as what it asked for comes in
        const row = (k) => box.locator(`.plugin-option[data-key="${k}"]`);
        assert.equal(await row("free_only").locator(".n").textContent(), w.label);
        assert.equal(await row("free_only").locator(".hint").textContent(), w.about);
        assert.equal(await row("beta").locator(".n").textContent(), w.beta);

        // the Models field: its label, then the chips, the note under them
        const models = ed.locator("label", { hasText: new RegExp("^" + w.models + "$") }).first();
        await models.waitFor();
        const field = models.locator("xpath=following-sibling::div[1]");
        const notes = await field.locator(":scope > .hint").allTextContents();
        if (on) {
          assert.deepEqual(notes, [w.note]);
          const where = await field.evaluate((f) => {
            const hint = f.querySelector(":scope > .hint"), chips = f.firstElementChild, r = f.closest(".editor").getBoundingClientRect(), h = hint.getBoundingClientRect();
            return { below: h.top >= chips.getBoundingClientRect().bottom - 1, spill: h.right - r.right, page: document.documentElement.scrollWidth - document.documentElement.clientWidth };
          });
          assert.ok(where.below, "the note is under the chips");
          assert.ok(where.spill <= 0, "the note runs past the editor by " + where.spill);
          assert.ok(where.page <= 0, "the page scrolls sideways by " + where.page);
          if (process.env.ARTIFACT_DIR) await field.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `plugin-option-words-${engine}-${width}-${lang}.png`) });
        } else {
          assert.deepEqual(notes, [], "nothing said while the switch is off");
        }

        // its confirm in the reader's language too
        await row("beta").locator(".opt", { hasText: w.on }).click();
        const dialog = page.locator("dialog.action-confirm");
        await dialog.waitFor();
        assert.equal(await dialog.locator("p").textContent(), w.ask);
        assert.ok((await dialog.locator("h2").textContent()).includes(w.beta));
        assert.deepEqual(errors, []);
        await ctx.close();
      });
    }
  });
}
