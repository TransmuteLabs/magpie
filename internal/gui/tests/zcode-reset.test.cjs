// The real lazy ZCode account sheet: confirmation freezes the named login,
// a rejected generation disables mutation, and an unknown reset cannot spend again.
// Run through make test-ui with this file and UI_TEST_CONCURRENCY=1.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const assets = path.resolve(__dirname, "../assets");
const user = "alice@example.test";
const expiry = Date.now() + 86400000;

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  test(`${engine}: native ZCode confirmation keeps scope and uncertain resets cannot repeat`, { timeout: 60000 }, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const context = await browser.newContext({ viewport: { width: 960, height: 900 }, reducedMotion: "reduce" });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [], changes = [], signins = [], legacySignins = [];
    let generation = "epoch-a", behavior = "stale", pending = false;
    const captured = () => ({ scope: "scope-alice-personal", generation, origin: "https://zcode.z.ai", family: "zai", userId: "alice-id", kind: "individual" });
    const reset = () => ({ source: "zcode", available: true, fiveHour: [expiry], week: [expiry], ...(pending ? { pending: "unconfirmed", pendingWindow: "week" } : {}) });
    const provider = { id: "zcode", name: "ZCode", icon: "zcode", models: [{ id: "GLM-5.3", on: true }], agents: [], fallback: [], headers: {}, keyList: [], account: { agent: "zcode", builtin: "zcode", agentName: "ZCode", user, plan: "GLM Coding Pro", logins: [{ user, active: true, on: true }] } };
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      const json = (value) => route.fulfill({ json: value });
      if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:true};' });
      if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
      if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang: "en", theme: "light", quotaLeft: true } });
      if (url.pathname === "/api/settings") return json({ lang: "en", theme: "light", quotaLeft: true, version: "fixture", dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425", fx: { rate: 1, stale: false } });
      if (url.pathname === "/api/providers") return json({ providers: [provider], presets: [], excluded: [], onPlugins: [], plugins: [{ id: "zcode", pid: "zcode", name: "ZCode", spec: "builtin:zcode", methods: [{ type: "oauth", label: "ZCode: Z.ai GLM Coding Plan" }], signedIn: true }], gateway: { running: true, window: true } });
      if (url.pathname === "/api/plugin-signin/prompt") return json({});
      if (url.pathname === "/api/plugin-signin") {
        signins.push(request.postDataJSON());
        return json({ agent: "zcode", state: "failed", error: "Fixture stops before remote authorization" });
      }
      if (url.pathname === "/api/signin") {
        legacySignins.push(request.postDataJSON());
        return json({ agent: "zcode", state: "failed", error: "Retired built-in sign-in must not run" });
      }
      if (url.pathname === "/api/login/usage") return json({ [user]: { provider: "zcode", user, windows: [], zcodeReset: reset() } });
      if (url.pathname === "/api/usage/zcode-accounts") return json({ available: true, accounts: [{ provider: "zcode", account: "zcode", user, name: "ZCode" }] });
      if (url.pathname === "/api/usage/zcode-account") {
        const input = request.postDataJSON();
        if (input.operation === "reset.use") {
          changes.push(input);
          if (behavior === "stale") return route.fulfill({ status: 409, json: { error: "The account changed", accountError: { kind: "account_generation_changed", dispatched: false } } });
          pending = true;
          return json({ context: captured(), data: reset() });
        }
        const data = input.operation === "context" ? { accountSelector: user }
          : input.operation === "selection.list" ? { selections: [] }
            : input.operation === "entitlement" ? { kind: "available", subscription: { productName: "GLM Coding Pro", status: "VALID" } }
              : input.operation === "usage.balance" ? { kind: "available", observations: [{ kind: "start-balance", name: "Start bucket", used: 3, limit: 10, remaining: 7, unit: "tokens", models: ["GLM-5.3"], identity: { planId: "plan-a", bucketId: "bucket-a", periodStart: "2026-11-01T00:00:00Z", periodEnd: "2026-12-01T00:00:00Z" } }] }
                : input.operation === "reset.status" ? reset() : {};
        return json({ context: captured(), data });
      }
      if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
      if (url.pathname === "/api/plugins") return json({ plugins: [] });
      if (url.pathname.startsWith("/api/")) return json(url.pathname === "/api/usage/quotas" ? [] : {});
      const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
      const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
      return route.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType });
    });
    await page.goto("http://magpie.test/?view=providers");
    await page.locator(".row.provider", { hasText: "ZCode" }).first().click();
    await page.getByText("Add another ZCode (GLM Coding Plan) account", { exact: true }).click();
    await page.locator('.signing.site-pick button[data-site="zai"]').click();
    await page.locator(".signing.failed").waitFor();
    assert.equal(signins.length, 1);
    assert.equal(signins[0].provider, "zcode");
    assert.equal(signins[0].method, 0);
    assert.deepEqual(legacySignins, []);
    await page.locator(".signing.failed").getByRole("button", { name: "Cancel", exact: true }).click();
    await page.locator(".editor .acc-more").click();
    await page.locator(".editor .zcode-account-open").click();
    const sheet = page.locator(".editor.zcode-account");
    const weekly = () => sheet.getByRole("button", { name: "Use weekly reset", exact: true });
    await weekly().waitFor();
    assert.match(await sheet.textContent(), /alice@example\.test/);
    assert.match(await sheet.textContent(), /Start bucket/);
    assert.match(await sheet.textContent(), /bucket-a/);
    for (const name of ["Account", "Reports", "Gifts", "Execution"]) assert.equal(await sheet.getByRole("button", { name, exact: true }).count(), 1);

    await weekly().click();
    const confirmation = page.locator("dialog.action-confirm");
    await confirmation.waitFor();
    assert.match(await confirmation.textContent(), /alice-id/);
    await confirmation.getByRole("button", { name: "Cancel", exact: true }).click();
    await confirmation.waitFor({ state: "detached" });
    assert.deepEqual(changes, []);

    await weekly().click();
    await confirmation.waitFor();
    generation = "epoch-b"; // the server switches while the captured confirmation is open
    await confirmation.getByRole("button", { name: "Use weekly reset", exact: true }).click();
    await confirmation.waitFor({ state: "detached" });
    await sheet.getByText("This account changed. Refresh the account before making changes.", { exact: false }).waitFor();
    assert.equal(changes.length, 1);
    assert.equal(changes[0].account, "zcode");
    assert.equal(changes[0].scope, "scope-alice-personal");
    assert.equal(changes[0].generation, "epoch-a");
    assert.deepEqual(changes[0].params, { window: "week" });
    assert.equal(await weekly().isDisabled(), true);

    behavior = "pending";
    await sheet.getByRole("button", { name: "Refresh account", exact: true }).click();
    await weekly().waitFor();
    await weekly().click();
    await confirmation.waitFor();
    await confirmation.getByRole("button", { name: "Use weekly reset", exact: true }).click();
    await confirmation.waitFor({ state: "detached" });
    await sheet.getByRole("button", { name: "Resolve uncertainty", exact: true }).waitFor();
    assert.equal(changes.length, 2);
    assert.equal(changes[1].generation, "epoch-b");
    assert.equal(await sheet.getByRole("button", { name: "Use weekly reset", exact: true }).count(), 0);
    assert.equal(await sheet.getByRole("button", { name: "Use five-hour reset", exact: true }).count(), 0);
    assert.match(await sheet.textContent(), /Reset outcome unconfirmed/);
    assert.deepEqual(errors, []);
    if (process.env.ARTIFACT_DIR) {
      await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `zcode-${engine}.png`) });
    }
    await sheet.getByRole("button", { name: "Close", exact: true }).click();
    await sheet.waitFor({ state: "detached" });
  });
}
