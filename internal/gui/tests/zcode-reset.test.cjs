// The real lazy ZCode account sheet: confirmation freezes the named login,
// a rejected generation disables mutation, and an unknown reset cannot spend again.
// Run through make test-ui with this file and UI_TEST_CONCURRENCY=1.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { openPart } = require("./reader.cjs");
const assets = path.resolve(__dirname, "../assets");
const user = "alice@example.test";
const expiry = Date.now() + 86400000;

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  test(`${engine}: native controls stop at the direct owner`, { timeout: 60000 }, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      await t.test(lang, async () => {
        const context = await browser.newContext({ viewport: { width: 960, height: 900 }, reducedMotion: "reduce" });
        const page = await context.newPage();
        page.setDefaultTimeout(5000);
        const requests = [], errors = [];
        let pendingRename = false;
        page.on("pageerror", (error) => errors.push(error.message));
        const cards = [
          { provider: "zcode", user: "local" },
          { provider: "office/zcode", user: "direct", from: "Office" },
          { provider: "office/branch/zcode", user: "nested", from: "Branch" },
          { provider: "zcode-plugin", user: "alias" },
        ].map((q) => ({ ...q, name: "ZCode", icon: "zcode", windows: [{ name: "5 hours", used: 17 }],
          observations: [{ kind: "server-mcp", name: "Server MCP", remaining: 16 }],
          resets: { source: "zcode", byWindow: true, count: 1, fiveHour: 1 },
          zcodeReset: { source: "zcode", available: true, fiveHour: [expiry], week: [] } }));
        await page.route("**/*", async (route) => {
          const request = route.request(), url = new URL(request.url());
          const json = (value) => route.fulfill({ json: value });
          if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:false};` });
          if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
          if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
          if (url.pathname === "/api/settings") return json({ lang, theme: "light", tray: "panel", trayUsages: [], fx: { rate: 1, stale: false } });
          if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], plugins: [], gateway: { running: true, window: true } });
          if (url.pathname === "/api/usage/quotas") return json(cards);
          if (url.pathname === "/api/usage/zcode-accounts") return json({ available: true, accounts: pendingRename
            ? [{ provider: "zcode", account: "zcode#pending", user: "local", error: "account rename is pending; repair the account settings and retry" },
              { provider: "zcode", account: "zcode#vendor", user: "local", error: "vendor exception #123" },
              { provider: "zcode", account: "zcode", user: "local" }]
            : [{ provider: "zcode", account: "zcode", user: "local" }] });
          if (url.pathname === "/api/usage/zcode-account") {
            const input = request.postDataJSON();
            requests.push(input);
            return json({ context: { scope: "fixture-scope", generation: "fixture-generation", userId: input.account, kind: "individual" },
              data: input.operation === "selection.list" ? { selections: [] } : {} });
          }
          if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
          if (url.pathname.startsWith("/api/")) return json({});
          const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
          const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
          return route.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType });
        });
        try {
          await page.goto("http://magpie.test/?view=usage");
          const card = (id) => page.locator(`.subscription-card[data-provider="${id}"]`);
          const nested = card("office/branch/zcode");
          const disabled = nested.locator(".zcode-account-open");
          await disabled.waitFor();
          assert.equal(await disabled.isDisabled(), true);
          const explanation = await disabled.getAttribute("title");
          assert.ok(explanation);
          if (lang !== "en") assert.notEqual(explanation, "Open this account on its directly connected owner to use native controls.");
          assert.ok((await disabled.textContent()).includes(explanation), "the owner explanation is visible");
          assert.match(await nested.textContent(), /Server MCP/);
          assert.match(await nested.textContent(), /16/);
          assert.equal(await nested.locator(".resets-words").count(), 1);
          await disabled.dispatchEvent("click");
          assert.deepEqual(requests, []);
          assert.equal(await card("zcode-plugin").locator(".zcode-account-open").count(), 0);
          pendingRename = true;
          await card("zcode").locator(".zcode-account-open").click();
          const pending = page.locator(".proto-menu .pm-item", { hasText: "zcode#pending" });
          await pending.waitFor();
          assert.equal(await pending.isDisabled(), true);
          const note = await pending.getAttribute("title");
          assert.ok(note);
          if (lang !== "en") assert.notEqual(note, "account rename is pending; repair the account settings and retry");
          assert.equal(await page.locator(".proto-menu .pm-item", { hasText: "zcode#vendor" }).getAttribute("title"), "vendor exception #123");
          assert.deepEqual(requests, []);
          await page.keyboard.press("Escape");
          pendingRename = false;
          for (const [id, account] of [["zcode", "zcode"], ["office/zcode", "direct"]]) {
            const button = card(id).locator(".zcode-account-open");
            await button.waitFor();
            assert.equal(await button.isDisabled(), false);
            await button.click();
            const sheet = page.locator(".editor.zcode-account");
            await sheet.waitFor();
            await sheet.locator(".zcode-section").first().waitFor();
            const call = requests.find((r) => r.provider === id && r.operation === "context");
            assert.ok(call, `${id} did not reach its account owner`);
            assert.equal(call.account, account);
            assert.equal(requests.some((r) => r.provider === "office/branch/zcode" || r.provider === "zcode-plugin"), false);
            await sheet.locator(".bar button").last().click();
            await sheet.waitFor({ state: "detached" });
          }
          assert.deepEqual(errors, []);
        } finally {
          await context.close();
        }
      });
    }
  });
  test(`${engine}: partial ZCode data and failure reach account, Usage and panel`, { timeout: 60000 }, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const context = await browser.newContext({ viewport: { width: 960, height: 900 }, reducedMotion: "reduce" });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    let mode = "mixed";
    const observation = { kind: "server-mcp", name: "Server MCP", used: 4, limit: 20, remaining: 16, unit: "" };
    const quota = () => ({
      provider: mode === "remote" ? "office/zcode" : "zcode", name: "ZCode", icon: "zcode", user,
      windows: mode === "mcp" || mode === "full" ? [] : [{ name: "Start", used: 40 }],
      error: mode === "mixed" ? "sign-in has expired" : "Coding Plan quota unavailable",
      partial: mode !== "full", pluginError: true, readAt: new Date().toISOString(),
      observations: mode === "full" ? [] : [observation],
      ...(mode === "remote" ? { from: "Office", resets: { source: "zcode", byWindow: true, count: 2, fiveHour: 1, weekly: 1 } } : {}),
      zcodeReset: { source: "zcode", available: false, error: "offline" },
    });
    const provider = { id: "zcode", name: "ZCode", icon: "zcode", models: [], agents: [], fallback: [], headers: {}, keyList: [],
      account: { agent: "zcode", builtin: "zcode", agentName: "ZCode", user, logins: [{ user, active: true, on: true, lapsed: "sign-in has expired" }] } };
    await page.route("**/*", async (route) => {
      const url = new URL(route.request().url());
      const json = (data) => route.fulfill({ json: data });
      if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:false};' });
      if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
      if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang: "en", theme: "light" } });
      if (url.pathname === "/api/settings") return json({ lang: "en", theme: "light", tray: "panel", trayUsages: [], fx: { rate: 1, stale: false } });
      if (url.pathname === "/api/providers") return json({ providers: [provider], presets: [], excluded: [], onPlugins: [],
        plugins: [{ id: "zcode", pid: "zcode", name: "ZCode", spec: "builtin:zcode", signedIn: true }], gateway: { running: true, window: true } });
      if (url.pathname === "/api/login/usage") return json({ [user]: quota() });
      if (url.pathname === "/api/usage/quotas") return json([quota()]);
      if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
      if (url.pathname === "/api/plugins") return json({ plugins: [] });
      if (url.pathname.startsWith("/api/")) return json({});
      const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
      const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
      return route.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType });
    });
    await page.goto("http://magpie.test/?view=providers");
    await page.locator(".row.provider", { hasText: "ZCode" }).first().click();
    const row = page.locator(".editor .aq").first();
    await row.getByText("Start", { exact: true }).waitFor();
    assert.match(await row.textContent(), /Sign in again/);
    assert.match(await row.textContent(), /40%/);

    // Finish lazy Settings reads before replacing their document: WebKit
    // reports their navigation cancellation through pageerror.
    await page.waitForLoadState("networkidle");
    await page.goto("http://magpie.test/?view=usage");
    const card = page.locator('#subscriptionUsage .subscription-card[data-provider="zcode"]');
    await card.locator(".zcode-observation").waitFor();
    assert.match(await card.textContent(), /Sign in again/);
    assert.match(await card.textContent(), /Start/);
    assert.match(await card.textContent(), /40%/);
    assert.match(await card.textContent(), /Server MCP/);
    assert.match(await card.textContent(), /Updated/);

    mode = "mcp";
    await page.waitForLoadState("networkidle");
    await page.goto("http://magpie.test/?mode=panel");
    await page.locator('#ptabs [data-ptab="usage"]').click();
    const panel = page.locator(".pq-card", { hasText: user });
    await panel.locator(".zcode-observation").waitFor();
    const text = await panel.textContent();
    assert.match(text, /Coding Plan quota unavailable/);
    assert.match(text, /Server MCP/);
    assert.match(text, /16/);
    assert.doesNotMatch(text, /Not used yet|0%/);
    assert.equal(await panel.locator(".pq-ring").count(), 0);

    mode = "full";
    await page.waitForLoadState("networkidle");
    await page.reload();
    await page.locator('#ptabs [data-ptab="usage"]').click();
    const failed = page.locator(".pq-card", { hasText: user });
    await failed.locator(".pq-sub.err").waitFor();
    assert.match(await failed.textContent(), /Coding Plan quota unavailable/);
    assert.equal(await failed.locator(".pq-ring, .zcode-observation").count(), 0);

    mode = "remote";
    await page.waitForLoadState("networkidle");
    await page.reload();
    await page.locator('#ptabs [data-ptab="usage"]').click();
    const remote = page.locator(".pq-card", { hasText: user });
    await remote.locator(".pq-resets .resets-words").waitFor();
    assert.match(await remote.textContent(), /1 five-hour reset.*1 weekly reset/);
    assert.match(await remote.textContent(), /Coding Plan quota unavailable/);
    assert.match(await remote.textContent(), /40%/);
    assert.equal(await remote.getByRole("button", { name: /Use.*reset/i }).count(), 0);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: native ZCode confirmation keeps scope and uncertain resets cannot repeat`, { timeout: 60000 }, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const context = await browser.newContext({ viewport: { width: 960, height: 900 }, reducedMotion: "reduce" });
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const errors = [], changes = [], signins = [], legacySignins = [];
    let generation = "epoch-a", behavior = "stale", pending = false, renamePending = false, accountReads = 0;
    const captured = () => ({ scope: "scope-alice-personal", generation, origin: "https://zcode.z.ai", family: "zai", userId: "alice-id", kind: "individual" });
    const reset = () => ({ source: "zcode", available: true, fiveHour: [expiry], week: [expiry], ...(pending ? { pending: "unconfirmed", pendingWindow: "week" } : {}) });
    const quota = () => ({
      provider: "zcode", name: "ZCode", icon: "zcode", user, plan: "GLM Coding Pro",
      windows: [{ name: "5 hours", used: 17, remaining: 83, resetsAt: new Date(expiry).toISOString() }],
      resets: { source: "zcode", count: 2, byWindow: true, fiveHour: 1, weekly: 1, until: new Date(expiry).toISOString(),
        each: [{ window: "fiveHour", until: new Date(expiry).toISOString() }, { window: "weekly", until: new Date(expiry).toISOString() }] },
      zcodeReset: reset(),
    });
    const provider = { id: "zcode", name: "ZCode", icon: "zcode", models: [{ id: "GLM-5.3", on: true }], agents: [], fallback: [], headers: {}, keyList: [], account: { agent: "zcode", builtin: "zcode", agentName: "ZCode", user, plan: "GLM Coding Pro", logins: [{ user, active: true, on: true }] } };
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request(), url = new URL(request.url());
      const json = (value) => route.fulfill({ json: value });
      if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:false};' });
      if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
      if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang: "en", theme: "light", quotaLeft: true } });
      if (url.pathname === "/api/settings") return json({ lang: "en", theme: "light", quotaLeft: true, tray: "panel", trayUsages: ["zcode|" + user], version: "fixture", dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425", fx: { rate: 1, stale: false } });
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
      if (url.pathname === "/api/login/usage") return json({ [user]: quota() });
      if (url.pathname === "/api/usage/quotas") return json([quota()]);
      if (url.pathname === "/api/usage/zcode-accounts") return json({ available: true, accounts: renamePending
        ? [{ provider: "zcode", account: "zcode", user, error: "account rename is pending; repair the account settings and retry" }, { provider: "zcode", account: "zcode#other", user }]
        : [{ provider: "zcode", account: "zcode", user, name: "ZCode" }] });
      if (url.pathname === "/api/usage/zcode-account") {
        accountReads++;
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
    renamePending = true;
    await page.locator(".editor .zcode-account-open").click();
    const pendingChoice = page.locator(".proto-menu .pm-item", { hasText: "zcode" }).filter({ hasText: "account rename is pending" });
    await pendingChoice.waitFor();
    assert.equal(await pendingChoice.isDisabled(), true);
    assert.match(await pendingChoice.textContent(), /account rename is pending/);
    assert.equal(accountReads, 0, "a pending selection must not reach native controls");
    await page.keyboard.press("Escape");
    renamePending = false;
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

    await page.goto("http://magpie.test/?view=usage");
    const summary = page.locator(".quota-resets .resets-words");
    await summary.waitFor();
    assert.equal(await summary.count(), 1);
    assert.match(await summary.textContent(), /1 five-hour reset.*1 weekly reset/);
    assert.match(await summary.getAttribute("title"), /Five-hour reset runs out/);
    assert.doesNotMatch(await summary.getAttribute("title"), /used on bigmodel.cn or z.ai/);
    assert.equal(await page.getByRole("button", { name: "Use a reset", exact: true }).count(), 0);
    assert.equal(await page.locator(".zcode-resets").getByText("Reset outcome unconfirmed", { exact: false }).count(), 1);
    assert.doesNotMatch(await page.locator(".zcode-resets").textContent(), /^\s*·/);
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `zcode-usage-${engine}.png`) });

    await page.locator("#prefs").click();
    await openPart(page, "usage");
    await page.locator("#trayUsagePick button").click();
    const selected = page.locator(".proto-menu .pm-item", { hasText: "ZCode" });
    await selected.waitFor();
    assert.equal(await selected.getAttribute("aria-checked"), "true");
    await page.keyboard.press("Escape");

    await page.goto("http://magpie.test/?mode=panel");
    await page.locator('#ptabs [data-ptab="usage"]').click();
    const panel = page.locator(".pq-resets .resets-words");
    await panel.waitFor();
    assert.equal(await panel.count(), 1);
    assert.match(await panel.textContent(), /1 five-hour reset.*1 weekly reset/);
    assert.equal(changes.length, 2, "displaying reset inventory never spends another reset");
    assert.deepEqual(errors, []);
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `zcode-panel-${engine}.png`) });
  });
}
