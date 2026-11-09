// OpenCode's add tiles start Console device OAuth instead of opening a
// key editor. Exercise waiting and completion without spurious cancellation;
// all HTTP and account data are local fixtures.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_OAUTH_ASSETS || path.resolve(__dirname, "../assets");
const presets = [
  { id: "opencode-go", name: "OpenCode Go", kind: "relay", icon: "opencode", oauth: true, chat: "https://opencode.ai/zen/go/v1" },
  { id: "opencode-zen", name: "OpenCode Zen", kind: "relay", icon: "opencode", oauth: true, noKey: true, chat: "https://opencode.ai/zen/v1" },
];

function fixture(lang, legacyID) {
  const settings = { lang, theme: "light", signInLinkOnly: true };
  const state = { started: [], canceled: [], complete: false, flow: null };
  const legacy = legacyID && { ...presets.find((pr) => pr.id === legacyID), preset: legacyID,
    models: [], agents: [], ready: true, key: { set: true, masked: "sk-…old" } };
  const account = () => {
    const pr = presets.find((pr) => pr.id === state.flow.agent);
    return { id: pr.id, name: pr.name, icon: pr.icon, models: [], agents: [], ready: true,
      account: { agent: pr.id, user: "member@example.com", plan: "Workspace", logins: [{ user: "member@example.com", plan: "Workspace", active: true, on: true }] } };
  };
  const serve = async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:${JSON.stringify(lang)},theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings });
    if (url.pathname === "/api/settings") return json(settings);
    if (url.pathname === "/api/providers") return json({ providers: state.complete ? [account()] : legacy ? [legacy] : [], presets, excluded: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/groups") return json({ groups: [], models: [], pools: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/gateway/trace") return json({ routes: [] });
    if (url.pathname === "/api/signin") {
      const body = JSON.parse(req.postData());
      state.started.push(body);
      state.flow = { id: "console-device", agent: body.agent, state: "waiting", code: "ABCD-EFGH", url: "https://opencode.ai/console/device?user_code=ABCD-EFGH", linkOnly: true };
      return json(state.flow);
    }
    if (url.pathname === "/api/signin/console-device/cancel") {
      state.canceled.push(state.flow.agent);
      return json({});
    }
    if (url.pathname === "/api/signin/console-device") return json(state.complete ? { ...state.flow, state: "done", user: "member@example.com", plan: "Workspace", using: true } : state.flow);
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    try { await route.fulfill({ body: await fs.readFile(file), contentType }); }
    catch { await route.fulfill({ status: 404, body: "" }); }
  };
  return { state, serve };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: OpenCode device OAuth`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      for (const pr of presets) {
        await t.test(`${lang}: ${pr.id}`, async (t) => {
          const context = await browser.newContext({ viewport: { width: 440, height: 900 }, reducedMotion: "reduce" });
          t.after(() => context.close());
          const page = await context.newPage();
          page.setDefaultTimeout(5000);
          const errors = [], { state, serve } = fixture(lang);
          page.on("pageerror", (e) => errors.push(e.message));
          await page.route("**/*", serve);
          await page.goto("http://magpie.test/?view=providers");
          const sheet = page.locator("#addSheet");
          const tile = sheet.locator(`.tile[data-pick="${pr.name}"]`);
          await tile.waitFor();
          assert.equal(await tile.count(), 1, "one OAuth entry, not an additional key tile");
          await tile.click();
          assert.equal(await page.locator(".editor input[type=password]").count(), 0, "OAuth never asks for an API key");
          const signing = sheet.locator(".signing");
          await signing.getByText("ABCD-EFGH", { exact: true }).waitFor();
          assert.deepEqual(state.started, [{ agent: pr.id }]);
          state.complete = true;
          await page.locator(`#providers .row[data-id="${pr.id}"]`).waitFor();
          assert.match(await page.locator(`#providers .row[data-id="${pr.id}"]`).textContent(), /member@example\.com/);
          assert.deepEqual(state.canceled, [], "a completed device sign-in is not canceled");
          assert.deepEqual(errors, []);
        });
      }
    }
    for (const action of ["Sign in", "Add another OpenCode Go"]) {
      await t.test(`legacy key: ${action}`, async (t) => {
        const context = await browser.newContext({ viewport: { width: 1100, height: 900 }, reducedMotion: "reduce" });
        t.after(() => context.close());
        const page = await context.newPage();
        page.setDefaultTimeout(5000);
        const { state, serve } = fixture("en", "opencode-go");
        await page.route("**/*", serve);
        await page.goto("http://magpie.test/?view=providers");
        await page.locator('#providers .row[data-id="opencode-go"]').click();
        await page.getByRole("button", { name: action, exact: true }).click();
        await page.locator("#addSheet .signing").getByText("ABCD-EFGH", { exact: true }).waitFor();
        await page.locator("#modal").waitFor({ state: "hidden" });
        assert.deepEqual(state.started, [{ agent: "opencode-go" }]);
      });
    }
  });
}
