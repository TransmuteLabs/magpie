const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { createHash } = require("node:crypto");
const { chromium, webkit } = require("playwright");
const { inEditor } = require("./reader.cjs");
const { isPageLifecycleRequest } = require("./fixtures/page-lifecycle.cjs");

const assets = path.resolve(__dirname, "../assets");
const sentinel = "sk-unsaved-synthetic-sentinel";
const endpoint = "https://relay.example.test/v1";
const fingerprint = (key) => createHash("sha256").update(key).digest("hex").slice(0, 10);
const actions = [
  ["P01", "Fetch models", "models"],
  ["P02", "Test models", "test"],
  ["P03", "Test this model", "test"],
  ["P04", "Test as Codex", "test"],
  ["P05", "Test as Claude Code", "test"],
  ["P06", "Work", "test"],
  ["P07", "Spare", "test"],
  ["P08", "Test", "test"],
  ["P09", "Detect APIs", "detect"],
  ["P10", "Each picked model", "detect"],
  ["P11", "Fetch models", "list"],
  ["P12", "Trust its certificate…", "peekcert"],
];

async function open(t, engine, lang, options = {}) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
  const page = await browser.newPage({ viewport: { width: 1000, height: 1000 }, reducedMotion: "reduce" });
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(60000);
  if (options.hold) await page.addInitScript((path) => {
    window.previewReplyRead = false;
    const original = window.fetch;
    window.fetch = async function (...args) {
      const response = await original.apply(this, args);
      if (new URL(response.url).pathname === path) {
        const text = response.text;
        response.text = function (...args) {
          return text.apply(this, args).then((value) => {
            setTimeout(() => { window.previewReplyRead = true; });
            return value;
          });
        };
      }
      return response;
    };
  }, options.hold);
  const posts = [], errors = [], logs = [], active = new Set();
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  t.after(async () => {
    release();
    try { await Promise.allSettled([...active]); }
    finally { await browser.close(); }
  });
  const relay = {
    id: "relay", name: "Relay", icon: "generic", chat: endpoint, responses: "", anthropic: "", catalog: "",
    models: [{ id: "gpt-5.1", name: "gpt-5.1", on: true }], agents: [], fallback: [], headers: { "X-Team": "a" },
    key: { set: true, masked: "sk-…one" },
    keyList: [{ id: "k1", name: "Work", masked: "sk-…one", on: true, active: true }, { id: "k2", name: "Spare", masked: "sk-…two", on: true }],
    testPicks: [{ id: "k1", name: "Work" }, { id: "k2", name: "Spare" }],
    testsAs: { codex: "", "claude-code": "" }, takesClientCert: true, takesServerPin: true,
    balanceToken: { takes: false, set: false }, proxy: "",
  };
  if (options.preset) relay.preset = "relay";
  if (options.disableTests) relay.modelTest = "own-api";
  if (options.rotateKeys) {
    relay.models.push({ id: "model-other", name: "model-other", on: true });
    relay.keyList.forEach((key, index) => { key.id = fingerprint(index ? "sk-b" : "sk-a"); });
    relay.testPicks = relay.keyList.map(({ id, name }) => ({ id, name }));
    relay.accountProxies = { [fingerprint("sk-a")]: "direct" };
    relay.keyPriceRates = { [fingerprint("sk-a")]: 0.5 };
    relay.accountModels = { [fingerprint("sk-a")]: ["gpt-5.1"] };
  }
  const providers = { providers: [relay], presets: options.preset ? [{ id: "relay", name: "Relay", chat: endpoint, icon: "generic" }] : [], excluded: [], gateway: { running: true, window: true } };
  if (options.second) providers.providers.push({ ...structuredClone(relay), id: "other", name: "Other" });
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("console", (message) => logs.push(message.text()));
  const serve = async (route) => {
    const request = route.request(), url = new URL(request.url());
    const json = (data, status = 200) => route.fulfill({ json: data, status });
    if (request.method() === "POST") posts.push({ path: url.pathname, body: request.postDataJSON(), lifecycle: isPageLifecycleRequest(request) });
    if (url.pathname === options.hold) await held;
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json(providers);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/provider/models") {
      if (options.removeAfterFetch) relay.keyList = relay.keyList.filter((key) => key.id !== "k1");
      if (options.offAfterFetch) relay.keyList[0].on = false;
      return json({ count: 1, provider: relay });
    }
    if (url.pathname === "/api/provider/test") {
      if (options.refuse) return json({ error: "401 synthetic refusal" }, 400);
      const body = request.postDataJSON();
      return json({ results: (body.test || ["gpt-5.1"]).map((model) => ({ ok: true, model, ms: 1, protocol: "chat" })) });
    }
    if (url.pathname === "/api/provider/list") return json({ models: relay.models });
    if (url.pathname === "/api/provider/detect") {
      const result = { protocol: "chat", base: endpoint, ok: true, ms: 1, model: "gpt-5.1" };
      return json({ results: [result], models: [{ model: "gpt-5.1", results: [result] }] });
    }
    if (url.pathname === "/api/provider/peekcert") return json({ sha256: "a".repeat(64), subject: "fixture", issuer: "fixture", notAfter: "2030-01-01T00:00:00Z", checks: true });
    if (url.pathname === "/api/keys/replace") {
      if (options.rotateKeys) {
        const body = request.postDataJSON(), row = relay.keyList.find((key) => key.id === body.ref);
        assert.ok(row, "replacement names an existing key fingerprint");
        const next = fingerprint(body.key);
        row.id = next;
        for (const pick of relay.testPicks) if (pick.id === body.ref) pick.id = next;
        for (const name of ["accountProxies", "keyPriceRates", "accountModels"]) if (Object.hasOwn(relay[name], body.ref)) {
          relay[name][next] = relay[name][body.ref];
          delete relay[name][body.ref];
        }
      }
      return json(providers);
    }
    if (url.pathname === "/api/provider/accountmodels") {
      const body = request.postDataJSON();
      if (!relay.keyList.some((key) => key.id === body.account)) return json({ error: "no such account" }, 400);
      relay.accountModels[body.account] = body.allow;
      return json(providers);
    }
    if (url.pathname === "/api/provider/save") return json(providers);
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    await route.fulfill({ body: await fs.readFile(file), contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)] });
  };
  await page.route("**/*", (route) => {
    const work = serve(route);
    active.add(work);
    work.finally(() => active.delete(work)).catch(() => {});
    return work;
  });
  await page.goto("http://magpie.test/?view=providers");
  await page.locator(".row.provider", { hasText: "Relay" }).click();
  const editor = page.locator(".editor");
  const tr = (word) => page.evaluate((word) => t(word), word);
  const click = async (locator) => { await inEditor(page, locator); await locator.click(); };
  const edit = async () => {
    const ref = options.rotateKeys ? fingerprint("sk-a") : "k1";
    await click(editor.locator(`.acc[data-account-id="${ref}"] .key-edit`));
    const key = editor.locator(`.acc[data-account-id="${ref}"] input.key-value-in`);
    await key.fill(sentinel);
    return key;
  };
  const business = () => posts.filter((entry) => !entry.lifecycle);
  const privacy = async () => {
    assert.equal(await page.evaluate((secret) => JSON.stringify([providerDraftValue(), localStorage, sessionStorage]).includes(secret), sentinel), false, "pending secret is not serialized or stored");
    assert.equal(logs.some((message) => message.includes(sentinel)), false, "normal application logs do not publish the secret");
    assert.deepEqual(errors, []);
  };
  return { page, editor, tr, click, edit, business, privacy, release, posts };
}

for (const engine of ["chromium", "webkit"]) {
  test(`${engine} en common preview transaction held picks close A open B`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy, release } = await open(t, engine, "en", { hold: "/api/provider/accountmodels", second: true, rotateKeys: true });
    const ref = fingerprint("sk-a");
    await click(editor.locator(`.acc[data-account-id="${ref}"] .amodels`));
    await click(editor.locator(".acct-models .mchip").filter({ hasText: "model-other" }));
    await edit();
    const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/provider/accountmodels");
    await click(editor.locator(".bar").getByRole("button", { name: await tr("Save"), exact: true }));
    await request;
    await click(editor.locator(".bar .ed-cancel"));
    await page.getByRole("alertdialog").getByRole("button", { name: await tr("Discard"), exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    await page.locator(".row.provider", { hasText: "Other" }).click();
    await click(editor.locator(`.acc[data-account-id="${ref}"] .amodels`));
    const before = await page.evaluate(() => JSON.stringify({ form: providerDraftValue(), pick: acctModels }));
    const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/accountmodels");
    release();
    await (await response).finished();
    await page.waitForFunction(() => window.previewReplyRead);
    assert.equal(await page.evaluate(() => JSON.stringify({ form: providerDraftValue(), pick: acctModels })), before, "accepted picks A cannot clear B's live picker or continue Save in B");
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace", "/api/provider/accountmodels"], "only the already dispatched A effects complete");
    await privacy();
  });

  test(`${engine} en common preview transaction same owner menu after rotation`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy } = await open(t, engine, "en", { rotateKeys: true });
    await edit();
    const chip = editor.locator(".mchips .mchip", { hasText: "gpt-5.1" });
    await inEditor(page, chip);
    await chip.click({ button: "right" });
    await page.locator(".pop.row-menu").getByRole("menuitem", { name: await tr("Turn on image input"), exact: true }).click();
    await page.waitForFunction(() => draft?.modelPrefs?.["gpt-5.1"]?.images === true);
    assert.equal(await editor.locator(`.acc[data-account-id="${fingerprint(sentinel)}"]`).count(), 1, "expected same-owner redraw retains the accepted credential");
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace"]);
    await privacy();
  });

  test(`${engine} en common preview transaction accepted Save close A open B`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy, release } = await open(t, engine, "en", { hold: "/api/provider/save", second: true, rotateKeys: true });
    await edit();
    const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/provider/save");
    await click(editor.locator(".bar").getByRole("button", { name: await tr("Save"), exact: true }));
    await request;
    await click(editor.locator(".bar .ed-cancel"));
    await page.getByRole("alertdialog").getByRole("button", { name: await tr("Discard"), exact: true }).click();
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    await page.locator(".row.provider", { hasText: "Other" }).click();
    const before = await page.evaluate(() => providerDraftValue());
    const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/save");
    const state = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/state");
    release();
    await (await response).finished();
    await (await state).finished();
    assert.equal(await page.evaluate(() => providerDraftValue()), before, "accepted Save A does not clear or replace the current B editor");
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace", "/api/provider/save"], "accepted credential and Save effects finish once, without replay");
    await privacy();
  });

  test(`${engine} en common preview transaction rotated Save`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy } = await open(t, engine, "en", { rotateKeys: true });
    const ref = fingerprint("sk-a"), next = fingerprint(sentinel);
    await click(editor.locator(`.acc[data-account-id="${ref}"] .amodels`));
    await click(editor.locator(".acct-models .mchip").filter({ hasText: "model-other" }));
    await edit();
    const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/accountmodels");
    await click(editor.locator(".bar").getByRole("button", { name: await tr("Save"), exact: true }));
    await (await response).finished();
    const picks = business().find((entry) => entry.path === "/api/provider/accountmodels");
    assert.deepEqual(picks.body, { id: "relay", account: next, allow: ["gpt-5.1", "model-other"] }, "pending model picks follow the same accepted slot");
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    const save = business().find((entry) => entry.path === "/api/provider/save");
    assert.deepEqual(save.body.accountProxies, { [next]: "direct" }, "Save derives proxies on the accepted replacement fingerprint");
    assert.deepEqual(save.body.keyPriceRates, { [next]: 0.5 }, "Save retains rates after fingerprint migration");
    assert.equal(JSON.stringify(save.body).includes(ref), false, "Save cannot publish a retired slot reference");
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace", "/api/provider/accountmodels", "/api/provider/save"]);
    await privacy();
  });

  test(`${engine} en common preview transaction held menu close A open B`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy, release } = await open(t, engine, "en", { hold: "/api/keys/replace", second: true, rotateKeys: true });
    await edit();
    const chip = editor.locator(".mchips .mchip", { hasText: "gpt-5.1" });
    await inEditor(page, chip);
    await chip.click({ button: "right" });
    const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/keys/replace");
    await page.locator(".pop.row-menu").getByRole("menuitem", { name: await tr("Turn on image input"), exact: true }).click();
    await request;
    await click(editor.locator(".bar .ed-cancel"));
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    await page.locator(".row.provider", { hasText: "Other" }).click();
    const before = await page.evaluate(() => providerDraftValue());
    const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/keys/replace");
    release();
    await (await response).finished();
    await page.waitForResponse((response) => new URL(response.url()).pathname === "/api/state");
    assert.equal(await page.evaluate(() => providerDraftValue()), before, "the accepted A credential transaction never resumes its old menu action in B");
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace"]);
    await privacy();
  });

  test(`${engine} en common preview transaction two Save presses held`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy, release } = await open(t, engine, "en", { hold: "/api/keys/replace", rotateKeys: true });
    await edit();
    const button = editor.locator(".bar").getByRole("button", { name: await tr("Save"), exact: true });
    const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/keys/replace");
    await click(button);
    await request;
    await button.click();
    const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/save");
    release();
    await (await response).finished();
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace", "/api/provider/save"], "pending Save activations join exactly one owned transaction");
    await privacy();
  });
}

for (const engine of ["chromium", "webkit"]) for (const lang of ["en", "zh"]) {
  for (const [id, action, family] of actions) {
    test(`${engine} ${lang} ${id} common preview ${action}`, async (t) => {
      const fixture = await open(t, engine, lang, { preset: id === "P08" });
      const { page, editor, tr, click, edit, business, privacy } = fixture;
      if (id === "P11") {
        await editor.locator(".bar .ed-cancel").click();
        await page.locator("#addProvider").click();
        await page.locator("#addSheet .row-head .custom").click();
        await editor.locator('input[type="url"][placeholder="https://…/v1"]').first().fill(endpoint);
        await editor.locator('.pair input[type="password"]').first().fill(sentinel);
      } else {
        if (id === "P12" && !await editor.locator("details.advanced").evaluate((element) => element.open)) await click(editor.locator("details.advanced > summary"));
        await edit();
        await privacy();
      }
      const dispatch = async (target) => {
        await inEditor(page, target);
        const request = page.waitForRequest((request) => request.method() === "POST" && new URL(request.url()).pathname === "/api/provider/" + family);
        const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/" + family);
        const [sent, reply] = await Promise.all([request, response, target.click()]);
        await reply.finished();
        return sent.postDataJSON();
      };
      let sent;
      if (["P03", "P04", "P05", "P06", "P07"].includes(id)) {
        const chip = editor.locator(".mchips .mchip", { hasText: "gpt-5.1" });
        await inEditor(page, chip);
        await chip.click({ button: "right" });
        const menu = page.locator(".pop.row-menu");
        if (["P06", "P07"].includes(id)) {
          await menu.getByRole("menuitem", { name: await tr("Test on account…"), exact: true }).click();
          sent = await dispatch(page.locator(".model-account-menu .pm-item").filter({ hasText: action }));
        } else sent = await dispatch(menu.getByRole("menuitem", { name: await tr(action), exact: true }));
      } else {
        const target = id === "P08" ? editor.locator(".eps").getByRole("button", { name: await tr(action), exact: true })
          : id === "P12" ? editor.locator(".pin-peek")
          : id === "P11" ? editor.locator(".add-models .fetch-models")
          : editor.getByRole("button", { name: await tr(action), exact: true });
        sent = await dispatch(target);
      }
      assert.deepEqual(business().map((entry) => entry.path), ["/api/provider/" + family], "preview has no account/config write or second request");
      assert.equal(sent.typed, true);
      assert.equal(sent.key, id === "P07" ? "" : sentinel, "actual replacement is tried without a saved-key fallback");
      if (id === "P07") {
        assert.equal(sent.account, "k2");
        assert.equal(sent.typedKeyRef, undefined);
        assert.equal(JSON.stringify(sent).includes(sentinel), false);
      } else if (id !== "P11") assert.equal(sent.typedKeyRef, "k1");
      if (id === "P06") assert.equal(sent.account, "k1");
      if (!["P06", "P07"].includes(id)) assert.equal(sent.account, undefined, "an unpinned preview omits the account field");
      if (!["P04", "P05"].includes(id)) assert.equal(sent.as, undefined, "ordinary previews omit agent impersonation");
      if (id === "P04") assert.equal(sent.as, "codex");
      if (id === "P05") assert.equal(sent.as, "claude-code");
      if (id === "P08") assert.equal(sent.test, undefined);
      if (["P02", "P03", "P04", "P05", "P06", "P07"].includes(id)) assert.deepEqual(sent.test, ["gpt-5.1"]);
      if (id === "P10") assert.deepEqual(sent.detectModels, ["gpt-5.1"]);
      if (id !== "P11") await privacy();
    });
  }
}

for (const engine of ["chromium", "webkit"]) for (const action of ["enter", "tab", "save", "escape", "cancel-pointer", "refusal"]) {
  test(`${engine} en common preview owner ${action}`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy } = await open(t, engine, "en", { refuse: action === "refusal" });
    const key = await edit();
    if (action === "enter") await key.press("Enter");
    else if (action === "tab") await key.press("Tab");
    else if (action === "escape") {
      await key.press("Escape");
      await key.waitFor({ state: "detached" });
    } else if (action === "save") {
      const request = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/provider/save");
      const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/save");
      await click(editor.locator(".bar").getByRole("button", { name: await tr("Save"), exact: true }));
      await request;
      await (await response).finished();
    } else if (action === "cancel-pointer") {
      const button = editor.locator(".mfoot .fetch-models");
      await inEditor(page, button);
      const rect = await button.boundingBox();
      await page.mouse.move(rect.x + rect.width / 2, rect.y + rect.height / 2);
      await page.mouse.down();
      await page.mouse.move(1, 1);
      await page.mouse.up();
      assert.deepEqual(business(), [], "canceled preview press neither sends nor commits");
      assert.equal(await key.inputValue(), sentinel);
      const chip = editor.locator(".mchips .mchip", { hasText: "gpt-5.1" });
      await inEditor(page, chip);
      await chip.click({ button: "right" });
      await page.locator(".pop.row-menu").waitFor();
      await page.keyboard.press("Escape");
      assert.equal(await page.locator(".pop.row-menu").count(), 0);
    } else {
      await click(editor.locator(".mfoot").getByRole("button", { name: await tr("Test models"), exact: true }));
      await page.locator("#status.err").waitFor();
    }
    if (["enter", "tab"].includes(action)) {
      await key.waitFor({ state: "detached" });
      assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace"]);
      assert.deepEqual(business()[0].body, { id: "relay", ref: "k1", key: sentinel });
    } else if (action === "save") {
      assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace", "/api/provider/save"]);
      assert.equal(business()[1].body.key, "");
      assert.equal(JSON.stringify(business()[1].body).includes(sentinel), false);
    } else if (action === "refusal") {
      assert.deepEqual(business().map((entry) => entry.path), ["/api/provider/test"]);
      assert.equal(await key.inputValue(), sentinel);
    } else assert.deepEqual(business(), []);
    if (action !== "save") await privacy();
  });
}

for (const engine of ["chromium", "webkit"]) {
  test(`${engine}: a typed replacement key changes Close to Cancel until cleared`, async (t) => {
    const { page, editor, tr, click, edit, business, privacy } = await open(t, engine, "en");
    const button = editor.locator(".bar .ed-cancel");
    const close = await tr("Close"), cancel = await tr("Cancel");
    assert.equal(await button.textContent(), close);
    const key = await edit();
    await button.filter({ hasText: cancel }).waitFor({ timeout: 3000 });
    await key.fill("");
    await button.filter({ hasText: close }).waitFor({ timeout: 3000 });
    await click(button);
    await page.waitForFunction(() => document.querySelector("#modal").hidden);
    assert.deepEqual(business(), []);
    await privacy();
  });
}

for (const engine of ["chromium", "webkit"]) for (const action of ["disabled", "stop", "held-commit", "redraw", "close-delayed", "stale-slot"]) {
  test(`${engine} en common preview lifetime ${action}`, async (t) => {
    if (action === "stale-slot") {
      for (const change of ["removeAfterFetch", "offAfterFetch"]) await t.test(change, async (t) => {
        const { page, editor, tr, click, edit, business, privacy } = await open(t, engine, "en", { [change]: true });
        const key = await edit();
        const old = await key.elementHandle();
        const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/models");
        await click(editor.locator(".mfoot .fetch-models"));
        await (await response).finished();
        await page.waitForFunction((old) => !old.isConnected, old);
        await old.dispose();
        await click(editor.locator(".mfoot").getByRole("button", { name: await tr("Test models"), exact: true }));
        await page.locator("#status.err").waitFor();
        assert.deepEqual(business().map((entry) => entry.path), ["/api/provider/models"], "stale slot must refuse without saved-key fallback");
        await privacy();
      });
      return;
    }
    const hold = action === "stop" ? "/api/provider/test" : action === "held-commit" ? "/api/keys/replace" : action === "close-delayed" ? "/providerpreview.js" : undefined;
    const { page, editor, tr, click, edit, business, privacy, release } = await open(t, engine, "en", { hold, disableTests: action === "disabled" });
    if (action === "close-delayed") {
      const response = page.waitForResponse((response) => new URL(response.url()).pathname === hold);
      await click(editor.locator('.acc[data-account-id="k1"] .key-edit'));
      assert.equal(await editor.locator(".key-value-in").count(), 0);
      await click(editor.locator(".bar .ed-cancel"));
      release();
      await (await response).finished();
      await page.waitForFunction(() => document.querySelector("#modal").hidden);
      assert.equal(await page.locator(".key-value-in").count(), 0);
      assert.deepEqual(business(), []);
      return;
    }
    const key = await edit();
    if (action === "disabled") {
      const button = editor.locator(".mfoot").getByRole("button", { name: await tr("Test models"), exact: true });
      await inEditor(page, button);
      assert.equal(await button.isDisabled(), true);
      const rect = await button.boundingBox();
      await page.mouse.click(rect.x + rect.width / 2, rect.y + rect.height / 2);
      assert.deepEqual(business(), [], "disabled preview does not commit or dispatch");
    } else if (action === "stop") {
      const request = page.waitForRequest((request) => new URL(request.url()).pathname === hold);
      await click(editor.locator(".mfoot").getByRole("button", { name: await tr("Test models"), exact: true }));
      await request;
      const stopped = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/teststop");
      await click(editor.locator(".mfoot").getByRole("button", { name: await tr("Stop"), exact: true }));
      await (await stopped).finished();
      release();
      assert.deepEqual(business().map((entry) => entry.path), ["/api/provider/test", "/api/provider/teststop"]);
      assert.deepEqual(Object.keys(business()[1].body), ["run"]);
      assert.equal(JSON.stringify(business()[1].body).includes(sentinel), false);
    } else if (action === "held-commit") {
      const request = page.waitForRequest((request) => new URL(request.url()).pathname === hold);
      const response = page.waitForResponse((response) => new URL(response.url()).pathname === hold);
      await key.press("Enter");
      await request;
      await click(editor.locator(".mfoot .fetch-models"));
      await page.locator("#status.err").waitFor();
      assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace"], "no preview or replay against pending commit");
      await click(editor.locator(".bar .ed-cancel"));
      release();
      await (await response).finished();
      await page.waitForFunction(() => document.querySelector("#modal").hidden);
      assert.deepEqual(business().map((entry) => entry.path), ["/api/keys/replace"], "accepted commit keeps ownership after close");
    } else {
      const old = await key.elementHandle();
      const response = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/models");
      await click(editor.locator(".mfoot .fetch-models"));
      await (await response).finished();
      await page.waitForFunction((old) => !old.isConnected, old);
      await old.dispose();
      assert.equal(await key.inputValue(), sentinel);
      const tested = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/provider/test");
      await click(editor.locator(".mfoot").getByRole("button", { name: await tr("Test models"), exact: true }));
      await (await tested).finished();
      assert.deepEqual(business().map((entry) => entry.path), ["/api/provider/models", "/api/provider/test"]);
      assert.equal(business()[1].body.key, sentinel);
    }
    await privacy();
  });
}
