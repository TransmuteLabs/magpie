const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_ASSETS || path.resolve(__dirname, "../assets");
const bridge = path.resolve(__dirname, "../pagebridge.js");
const origin = "http://magpie.test";
const signInPath = "/api/provider/sdk-signin";
const authError = "module authentication could not complete this request";
const deviceURL = "https://vendor.test/device";
const selected = "0123456789abcdef0123456789abcdef";
const other = "other@example.test";
const selectedName = "selected@example.test";
const selectedAlias = "Selected local alias";
const device = { id: "device", mode: "poll", label: "Device code" };
const key = { id: "key", mode: "poll", label: "API key", fields: [
  { key: "site", type: "select", label: "Account region", choices: [
    { value: "global", label: "Global" }, { value: "eu", label: "Europe" },
  ] },
  { key: "key", type: "secret", label: "Provider secret" },
  { key: "name", type: "text", label: "Account label", optional: true },
] };
const primary = { id: "extension-0123456789abcdef12345678", name: "Fixture SDK", icon: "generic",
  revision: "a".repeat(64), reauthorize: true, methods: [device, key] };
const alternate = { id: "extension-fedcba9876543210fedcba98", name: "Alternate SDK", icon: "generic",
  revision: "b".repeat(64), methods: [device] };
const pageName = "sdk-signin-page", pageRef = "c".repeat(32);
const pageFolder = `/_magpie/plugin/${pageRef}/`;
const entry = { spec: `sdk:${pageName}@${"c".repeat(64)}:0`, name: pageName,
  key: "extension-" + Buffer.from(pageName).toString("hex"), title: "Legacy sign-in",
  ref: pageRef, reads: [], url: pageFolder + "index.html" };
const legacy = { id: "extension-abcdef0123456789abcdef01", name: "Legacy SDK", icon: "generic", page: entry.key };
const text = {
  en: { section: "Subscriptions", method: "Sign-in method", sign: "Sign in", cancel: "Cancel", close: "Close", again: "Sign in again", open: "Open in browser" },
  zh: { section: "订阅", method: "登录方式", sign: "登录", cancel: "取消", close: "关闭", again: "重新登录", open: "在浏览器中打开" },
  "zh-TW": { section: "訂閱", method: "登入方式", sign: "登入", cancel: "取消", close: "關閉", again: "重新登入", open: "在瀏覽器中開啟" },
  ja: { section: "サブスクリプション", method: "サインイン方法", sign: "サインイン", cancel: "キャンセル", close: "閉じる", again: "再度サインイン", open: "ブラウザで開く" },
  de: { section: "Abos", method: "Anmeldemethode", sign: "Anmelden", cancel: "Abbrechen", close: "Schließen", again: "Erneut anmelden", open: "Im Browser öffnen" },
};

function fixture(lang, subscription = primary) {
  const starts = [];
  const signouts = [];
  const flows = new Map();
  const opens = [];
  const unexpected = [];
  let logins = [];
  let accountName, accountAlias;
  let held, finish, linkOnly = true, reauthorize = true, legacyPage = false, pageListed = true;
  let owner = subscription.ownerRevision ? "sdk" : "";
  const lapse = "This sign-in has expired; sign in again";
  const payload = () => {
    const providers = [{ id: "openai", name: "OpenAI", icon: "openai", models: [], agents: [],
      key: { set: true, masked: "sk-…abcd" } }];
    if (logins.length) providers.push({ id: subscription.id, name: subscription.name, icon: subscription.icon,
      accountUsers: accountName ? { [selected]: accountName } : undefined,
      accountNames: accountAlias ? { [selected]: accountAlias } : undefined,
      models: [], agents: [], key: {}, account: { agent: subscription.id, agentName: subscription.name,
        agentIcon: subscription.icon, ownerRevision: subscription.ownerRevision, user: logins[0].user, logins } });
    const sdkSubscriptions = [
      ...(!owner || owner === "sdk" ? [{ ...subscription, reauthorize }] : []),
      alternate, ...(legacyPage ? [legacy] : []),
    ];
    const restored = owner === "community";
    const plugins = restored ? [{ id: subscription.id, pid: subscription.id, name: "Community Devin", spec: "@fixture/devin" }] : [];
    return { providers, sdkSubscriptions, presets: [], plugins,
      onPlugins: restored ? [subscription.id] : [],
      sdkOwnedSubscriptions: owner && !restored ? [subscription.id] : [],
      movable: restored ? [subscription.id] : [],
      excluded: [], gateway: { running: true, window: true } };
  };
  const serve = async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.origin !== origin || /^\/api\/(?:signin|plugin-signin)(?:\/|$)/.test(url.pathname)) {
      unexpected.push(req.method() + " " + req.url());
      return route.abort();
    }
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript",
      body: `window.bootPrefs = ${JSON.stringify({ lang, theme: "light", web: false })};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json(payload());
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [] });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/plugin-pages") return json(legacyPage && pageListed ? [entry] : []);
    if (url.pathname === "/api/plugin-panels") {
      return legacyPage ? route.fulfill({ status: 503, json: { error: "Panels unavailable" } }) : json([]);
    }
    if (url.pathname === "/api/open") {
      opens.push(req.postDataJSON().url);
      return json({});
    }
    if (url.pathname === "/api/login/forget") {
      const body = req.postDataJSON();
      const account = body.account;
      signouts.push(body);
      if (body.agent !== subscription.id || body.ownerRevision !== subscription.ownerRevision || !account || !logins.some((login) =>
        login.user === account.id && login.extensionGeneration === account.expectedGeneration)) {
        return route.fulfill({ status: 400, json: { error: "extension account generation changed" } });
      }
      logins = logins.filter((login) => login.user !== account.id).map((login, index) => ({ ...login, active: index === 0 }));
      return json(payload());
    }
    if (url.pathname === signInPath) {
      const body = req.postDataJSON();
      if (body.operation === "start") {
        starts.push(body);
        const account = body.options.account;
        if (body.ownerRevision !== subscription.ownerRevision || account && (!reauthorize || !logins.some((item) => item.user === account.id && item.extensionGeneration === account.expectedGeneration))) {
          return route.fulfill({ status: 400, json: { error: authError } });
        }
        const flow = `flow-${starts.length}`;
        const view = { flow, state: "waiting", url: deviceURL, userCode: `SDK-${flow}`, retryAfterMs: 1000, linkOnly };
        flows.set(flow, { start: body, view, canceled: false });
        const wait = held;
        held = null;
        if (wait) {
          wait.receive();
          await wait.response;
        }
        return json(view);
      }
      const flow = flows.get(body.flow);
      if (!flow || flow.start.parent !== body.parent || flow.start.id !== body.id || flow.start.revision !== body.revision ||
          flow.start.ownerRevision !== body.ownerRevision) {
        unexpected.push("SDK sign-in used the wrong flow identity");
        return route.fulfill({ status: 400, json: { error: "wrong flow" } });
      }
      if (body.operation === "cancel") {
        flow.canceled = true;
        return json({ state: "canceled" });
      }
      if (body.operation === "finish" && !flow.canceled) {
        if (body.flow !== finish) return json(flow.view);
        const renewing = flow.start.options.account;
        const account = renewing?.id || selected;
        const generation = renewing ? Number(renewing.expectedGeneration) + 1 : 4;
        const login = { user: account, active: !renewing, on: true, extensionGeneration: String(generation) };
        logins = renewing ? logins.map((item) => item.user === account ? login : item) : [login];
        return json({ flow: body.flow, state: "complete", account: { id: account, generation } });
      }
      unexpected.push(`SDK sign-in used ${body.operation} on a canceled flow`);
      return route.fulfill({ status: 400, json: { error: "closed flow" } });
    }
    if (url.pathname.startsWith("/api/")) return json({});
    if (legacyPage && url.pathname === entry.url) return route.fulfill({ contentType: "text/html",
      body: '<!doctype html><p hidden>Legacy sign-in is ready</p><script src="_magpie/bridge.js"></script><script src="ready.js"></script>' });
    if (legacyPage && url.pathname === pageFolder + "_magpie/bridge.js") {
      return route.fulfill({ contentType: "text/javascript", body: await fs.readFile(bridge) });
    }
    if (legacyPage && url.pathname === pageFolder + "ready.js") {
      return route.fulfill({ contentType: "text/javascript",
        body: 'magpie.ready.then(() => { document.querySelector("p").hidden = false; });' });
    }
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css",
      ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" }[path.extname(file)];
    const bytes = await fs.readFile(file).catch((error) => {
      if (error.code !== "ENOENT") throw error;
      return null;
    });
    return bytes ? route.fulfill({ body: bytes, contentType }) : route.fulfill({ status: 404, body: "" });
  };
  return { serve, starts, signouts, flows, opens, unexpected,
    delayStart() {
      linkOnly = false;
      let receive, release;
      const received = new Promise((resolve) => { receive = resolve; });
      const response = new Promise((resolve) => { release = resolve; });
      held = { receive, response };
      return { received, release() { linkOnly = true; release(); } };
    },
    complete(flow) { finish = flow; },
    needReauthorization() {
      linkOnly = false;
      logins = [
        { user: other, active: true, on: true, extensionGeneration: "7" },
        { ...logins[0], active: false, lapsed: lapse },
      ];
    },
    advanceGeneration() { logins[1].extensionGeneration = "5"; },
    setAccountLabel(name, alias) { accountName = name; accountAlias = alias; },
    noReauthorization() { reauthorize = false; logins[1].lapsed = lapse; },
    showLegacy() { legacyPage = true; },
    removeLegacyPage() { pageListed = false; },
    setOwner(value, signInLapse = lapse) {
      owner = value;
      logins = value === "sdk" ? [{ user: selected, active: true, on: true, extensionGeneration: "4", lapsed: signInLapse }] : [];
    },
  };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: SDK provider summary uses the selected account name`, async (t) => {
    const ctl = fixture("en");
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const context = await browser.newContext({ serviceWorkers: "block" });
    await context.route("**/*", ctl.serve);
    const page = await context.newPage();
    page.setDefaultTimeout(5000);
    const row = page.locator(`.row.provider[data-id="${primary.id}"]`);
    const openProviders = async () => {
      await page.goto(origin + "/?view=providers");
      return row.locator(".who .sub").textContent();
    };

    ctl.setOwner("sdk", "");
    ctl.setAccountLabel(selectedName);
    assert.equal(await openProviders(), `signed in as ${selectedName} · no models exposed`);
    await row.click();
    const account = page.locator(`.editor .accts .acc[data-account-id="${selected}"]`);
    assert.equal(await account.locator(".acct-who .rename").textContent(), selectedName);
    await account.locator(".acc-more").click();
    await account.getByRole("button", { name: "Sign out", exact: true }).click();
    const confirm = page.getByRole("alertdialog");
    const [request] = await Promise.all([
      page.waitForRequest(origin + "/api/login/forget"),
      confirm.getByRole("button", { name: "Sign out", exact: true }).click(),
    ]);
    assert.deepEqual(request.postDataJSON(), {
      agent: primary.id, account: { id: selected, expectedGeneration: "4" },
    });
    await row.waitFor({ state: "detached" });

    ctl.setOwner("sdk");
    ctl.setAccountLabel(selectedName, selectedAlias);
    assert.equal(await openProviders(), `${selectedAlias} is signed out · no models exposed`);
    ctl.setOwner("sdk", "");
    assert.equal(await openProviders(), `signed in as ${selectedAlias} · no models exposed`);
    ctl.setAccountLabel();
    assert.equal(await openProviders(), `signed in as ${selected} · no models exposed`);
    assert.deepEqual(ctl.unexpected, []);
  });

  test(`${engine}: SDK subscription sign-in through Providers`, { timeout: 120_000 }, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    await t.test("transferred subscription keeps one tile and the current sign-in owner", async (t) => {
      const subscription = { ...primary, id: "devin", name: "Fixture Devin", ownerRevision: "owner-2", methods: [device] };
      const ctl = fixture("en", subscription);
      ctl.setOwner("sdk");
      ctl.setAccountLabel(selectedName, selectedAlias);
      const context = await browser.newContext({ viewport: { width: 1080, height: 850 }, reducedMotion: "reduce", serviceWorkers: "block" });
      t.after(() => context.close());
      await context.route("**/*", ctl.serve);
      const page = await context.newPage();
      page.setDefaultTimeout(5000);
      const tiles = page.locator("#addSheet .tile");
      const add = async () => {
        await page.goto(origin + "/?view=providers");
        await page.locator("#addProvider").click();
        await tiles.filter({ hasText: alternate.name }).waitFor();
      };
      await add();
      assert.deepEqual(await tiles.filter({ hasText: "Devin" }).allTextContents(), [subscription.name]);
      await page.locator("#addSheet").getByRole("button", { name: "Close", exact: true }).click();
      await page.locator(".row.provider").filter({ hasText: subscription.name }).click();
      const editor = page.locator(".editor .accts");
      await editor.getByRole("button", { name: "Sign in again", exact: true }).click();
      await editor.locator(".signing").getByText(`Sign in again · ${selectedAlias}`, { exact: true }).waitFor();
      await editor.locator(".signing").getByRole("button", { name: "Sign in", exact: true }).click();
      await editor.locator(".signing .devcode").waitFor();
      assert.equal(ctl.starts.length, 1);
      assert.equal(ctl.starts[0].id, subscription.id);
      assert.equal(ctl.starts[0].ownerRevision, subscription.ownerRevision);
      assert.deepEqual(ctl.starts[0].options.account, { id: selected, expectedGeneration: "4" });
      ctl.setOwner("unavailable");
      await add();
      assert.equal(await tiles.filter({ hasText: "Devin" }).count(), 0);
      ctl.setOwner("community");
      await add();
      assert.equal(await tiles.filter({ hasText: "Devin" }).count(), 1);
      assert.equal(await tiles.filter({ hasText: subscription.name }).count(), 0);
      assert.deepEqual(ctl.unexpected, []);
    });
    for (const [lang, words] of Object.entries(text)) {
      await t.test(lang, { timeout: 24_000 }, async (t) => {
        const ctl = fixture(lang);
        const context = await browser.newContext({ viewport: { width: 1080, height: 850 }, reducedMotion: "reduce", serviceWorkers: "block" });
        t.after(() => context.close());
        await context.route("**/*", ctl.serve);
        const page = await context.newPage();
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        page.on("pageerror", (error) => errors.push(error.message));
        const sheet = page.locator("#addSheet"), sheetForm = sheet.locator(".signing");
        const editor = page.locator(".editor .accts");
        const tile = (sub) => sheet.locator(".tile", { hasText: sub.name });
        const button = (inBox, name) => inBox.getByRole("button", { name, exact: true });
        const ready = (flow) => sheetForm.locator(".devcode code", { hasText: `SDK-${flow}` }).waitFor();
        const request = (op, flow) => page.waitForRequest((req) => req.method() === "POST" &&
          req.url() === origin + signInPath && req.postDataJSON().operation === op && (!flow || req.postDataJSON().flow === flow));
        const sign = async (inBox) => {
          const [sent] = await Promise.all([request("start"), button(inBox, words.sign).click()]);
          return sent.postDataJSON();
        };
        const cancel = async (flow, action) => {
          const [sent] = await Promise.all([request("cancel", flow), action()]);
          assert.equal(sent.postDataJSON().parent, ctl.flows.get(flow).start.parent);
        };
        const showAdd = async () => {
          if (!await sheet.isVisible()) await page.locator("#addProvider").click();
          await tile(primary).waitFor();
        };

        await page.goto(origin + "/?view=providers");
        await showAdd();
        assert.equal(await tile(primary).count(), 1);
        assert.equal(await tile(primary).evaluate((node) => node.closest(".grid").previousElementSibling.querySelector("b").textContent), words.section);
        assert.equal(await page.locator(".row.provider", { hasText: primary.name }).count(), 0);
        await tile(primary).click();
        assert.equal(ctl.starts.length, 0);
        await button(sheetForm, words.method).click();
        await page.getByRole("menuitemradio", { name: key.label, exact: true }).click();
        await button(sheetForm, "Account region").click();
        await page.getByRole("menuitemradio", { name: "Europe", exact: true }).click();
        const secret = sheetForm.getByLabel("Provider secret");
        assert.equal(await secret.getAttribute("type"), "password");
        assert.equal(await secret.inputValue(), "");
        await secret.fill("fixture-only-secret");
        const delayed = ctl.delayStart();
        try {
          const sent = await sign(sheetForm);
          assert.match(sent.parent, /^[0-9a-f]{32}$/);
          assert.deepEqual(sent, { id: primary.id, revision: primary.revision, parent: sent.parent,
            operation: "start", request: "start", options: { method: "key", input: { site: "eu", key: "fixture-only-secret" } } });
          await delayed.received;
          await secret.waitFor({ state: "hidden" });
          await cancel("flow-1", async () => {
            await button(sheetForm, words.cancel).click();
            await sheetForm.waitFor({ state: "hidden" });
            delayed.release();
          });
          assert.equal(await editor.count(), 0);
        } finally {
          delayed.release();
        }

        await tile(primary).click();
        await sign(sheetForm);
        await ready("flow-2");
        await cancel("flow-2", () => tile(alternate).click());
        await sheetForm.getByText(new RegExp(alternate.name)).waitFor();
        assert.equal((await sign(sheetForm)).id, alternate.id);
        await ready("flow-3");
        await cancel("flow-3", () => button(sheet, words.close).click());
        await sheet.waitFor({ state: "hidden" });

        await showAdd();
        await tile(primary).click();
        await sign(sheetForm);
        await ready("flow-4");
        await cancel("flow-4", () => page.locator('#nav button[data-view="agents"]').click());
        await page.locator("#view-providers").waitFor({ state: "hidden" });
        await page.locator('#nav button[data-view="providers"]').click();
        await showAdd();
        await sheetForm.waitFor({ state: "hidden" });

        await tile(primary).click();
        const poll = await sign(sheetForm);
        assert.deepEqual(poll.options, { method: "device", input: {} });
        assert.notEqual(poll.parent, ctl.flows.get("flow-1").start.parent);
        await request("finish", "flow-5");
        assert.equal(await sheetForm.locator(".signlink code").textContent(), deviceURL);
        assert.deepEqual(ctl.opens, [], "link-only sign-in waited for a click, while polling");
        await Promise.all([page.waitForRequest(origin + "/api/open"), button(sheetForm, words.open).click()]);
        ctl.complete("flow-5");
        await editor.locator(`[data-account-id="${selected}"]`).waitFor();
        await sheet.waitFor({ state: "hidden" });
        assert.deepEqual(ctl.opens, [deviceURL]);
        assert.equal(await page.locator(".row.provider", { hasText: primary.name }).count(), 1);

        ctl.needReauthorization();
        if (lang === "en") ctl.setAccountLabel(selectedName);
        const openEditor = async () => {
          await page.goto(origin + "/?view=providers");
          await page.locator(".row.provider", { hasText: primary.name }).click();
        };
        await openEditor();
        const selectedRow = editor.locator(`[data-account-id="${selected}"]`);
        const renewForm = editor.locator(".signing");
        const selectedAccount = (label) => renewForm.getByText(`${words.again} · ${label}`, { exact: true }).waitFor();
        await button(selectedRow, words.again).click();
        await selectedAccount(lang === "en" ? selectedName : selected);
        if (lang === "en") {
          ctl.advanceGeneration();
          ctl.setAccountLabel(selectedName, selectedAlias);
          const stale = await sign(renewForm);
          assert.deepEqual(stale.options.account, { id: selected, expectedGeneration: "4" });
          await renewForm.getByText(authError, { exact: true }).waitFor();
          await button(renewForm, "Try again").click();
          await selectedAccount(selectedAlias);
          assert.equal(ctl.starts.length, 6, "Try again requires a new sign-in submission");
        }
        const [renew] = await Promise.all([sign(renewForm), page.waitForRequest(origin + "/api/open")]);
        assert.deepEqual(renew.options.account, { id: selected, expectedGeneration: lang === "en" ? "5" : "4" });
        ctl.complete(lang === "en" ? "flow-7" : "flow-6");
        await selectedRow.waitFor();
        await button(selectedRow, words.again).waitFor({ state: "hidden" });
        assert.equal(await editor.locator(`[data-account-id="${other}"]`).count(), 1);
        assert.deepEqual(ctl.opens, [deviceURL, deviceURL]);

        if (lang === "en") {
          ctl.noReauthorization();
          await openEditor();
          await selectedRow.getByText("Sign-in required", { exact: true }).waitFor();
          assert.equal(await button(selectedRow, words.again).count(), 0);

          const activeRow = editor.locator(`[data-account-id="${other}"]`);
          const confirmation = page.getByRole("alertdialog");
          await button(activeRow, `Settings for ${other}`).click();
          await button(activeRow, "Sign out").click();
          await confirmation.getByRole("heading").filter({ hasText: other }).waitFor();
          await button(confirmation, words.cancel).click();
          await confirmation.waitFor({ state: "detached" });
          assert.deepEqual(ctl.signouts, []);
          const signOut = async (row, user, generation, label = user) => {
            await button(row, "Sign out").click();
            await confirmation.getByRole("heading").filter({ hasText: label }).waitFor();
            const [sent] = await Promise.all([
              page.waitForRequest(origin + "/api/login/forget"),
              button(confirmation, "Sign out").click(),
            ]);
            assert.deepEqual(sent.postDataJSON(), { agent: primary.id, account: { id: user, expectedGeneration: generation } });
            await row.waitFor({ state: "detached" });
          };
          await signOut(activeRow, other, "7");
          await selectedRow.waitFor();
          await signOut(selectedRow, selected, "6", selectedAlias);
          await page.locator(".row.provider", { hasText: primary.name }).waitFor({ state: "detached" });

          ctl.showLegacy();
          await page.goto(origin + "/?view=providers");
          await showAdd();
          await page.locator(`#nav button[data-view="${entry.key}"]`).waitFor({ state: "attached" });
          await Promise.all([
            page.waitForResponse((response) => response.url() === origin + "/api/plugin-panels" && response.status() === 503),
            tile(legacy).click(),
          ]);
          const legacyFrame = `#view-${entry.key} iframe`;
          const frame = page.frameLocator(legacyFrame);
          await frame.getByText("Legacy sign-in is ready", { exact: true }).waitFor();

          await page.locator('#nav button[data-view="providers"]').click();
          await showAdd();
          await tile(legacy).waitFor();
          ctl.removeLegacyPage();
          const [listing] = await Promise.all([
            page.waitForResponse(origin + "/api/plugin-pages"),
            tile(legacy).click(),
          ]);
          assert.deepEqual(await listing.json(), []);
          await page.locator("#status", { hasText: "Plugin page is unavailable" }).waitFor();
          await page.locator(legacyFrame).waitFor({ state: "detached" });
          assert(await page.locator("#view-providers").isVisible());
          assert.equal(ctl.starts.length, 7, "a page-based subscription opens its own sign-in");
        }
        assert.deepEqual([...ctl.flows].filter(([, flow]) => flow.canceled).map(([id]) => id), ["flow-1", "flow-2", "flow-3", "flow-4"]);
        assert.deepEqual(ctl.unexpected, []);
        assert.deepEqual(errors, []);
      });
    }
  });
}
