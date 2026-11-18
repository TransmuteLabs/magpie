const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const options = [
  { value: "magpie/fixture/alpha", label: "Alpha", ref: "fixture/alpha" },
  { value: "magpie/fixture/beta", label: "Beta", ref: "fixture/beta" },
];
const descriptors = {
  pool: {
    id: "worker-tools", name: "Worker Tools", configFiles: [".worker-tools/config.json"],
    fields: [{ key: "subagent_pool", label: "worker pool" }],
  },
  complete: {
    id: "worker-pool", name: "Worker Pool", configFiles: [".worker-pool/config.json"],
    fields: [
      { key: "subagent_model", label: "subagent model" },
      { key: "subagent_pool", label: "subagent pool" },
      { key: "subagent_effort", label: "subagent effort" },
    ],
  },
};

function projected(descriptor) {
  const fields = descriptor.fields.map((field) => ({
    ...field, value: options[0].value, options,
  }));
  return {
    id: descriptor.id, name: descriptor.name, icon: "generic",
    path: "/fixture/" + descriptor.configFiles[0], wired: true, fields,
    native: {
      provider: "connected", runtime: "available",
      fields: Object.fromEntries(fields.map((field) => [
        field.key, { value: field.value, status: "connected" },
      ])),
    },
  };
}

function server(lang, descriptor, evidence) {
  let current = projected(descriptor);
  const settings = { lang, theme: "light", guideHidden: true };
  const snapshot = () => ({ agents: [current], profiles: [], settings });
  const reads = new Map([
    ["/api/settings", settings],
    ["/api/usage/quotas", []],
    ["/api/groups", { groups: [] }],
    ["/api/agents/cli", { agents: {}, pending: false }],
    ["/api/providers", { providers: [], presets: [], gateway: { running: true } }],
    ["/api/plugins", { plugins: [] }],
    ["/api/update", {}],
    ["/api/notice", {}],
    ["/api/usage", {}],
    ["/api/plugins/updates", { waiting: [] }],
    ["/api/plugin-panels", []],
    ["/api/plugin-pages", []],
    ["/api/gateway/trace", { routes: [] }],
    ["/api/model-loads", {}],
    ["/api/plugins/agents", []],
    ["/api/agents/install", []],
    ["/api/whatsnew", { show: false, releases: [], total: 0 }],
    ["/api/fonts", []],
    ["/api/davsync", { on: false }],
    ["/api/update/signing", { supported: false }],
    ["/api/cli", { exe: "/fixture/magpie", ours: true, shells: [] }],
    ["/api/autobackup", { on: false, backups: [] }],
    ["/api/datafolder", { config: "/fixture/config" }],
  ]);
  return async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = ${JSON.stringify({ lang, theme: "light", web: true })};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state" && request.method() === "GET") return route.fulfill({ json: snapshot() });
    if (url.pathname === "/api/set" && request.method() === "POST") {
      const body = request.postDataJSON();
      evidence.posts.push(body);
      current = structuredClone(current);
      const field = current.fields.find((item) => item.key === body.field);
      if (field && body.agent === descriptor.id) {
        field.value = body.value;
        current.native.fields[field.key].value = body.value;
      }
      return route.fulfill({ json: snapshot() });
    }
    if (url.pathname === "/api/agents/preview/" + descriptor.id && request.method() === "GET") {
      evidence.previews.push({ path: url.pathname, method: request.method() });
      return route.fulfill({ json: { changes: [] } });
    }
    if (url.pathname === "/api/plugin-pages/changes" && request.method() === "GET") return route.fulfill({ status: 503, body: "Fixture has no window notification service" });
    if (url.pathname === "/api/update/using" && request.method() === "POST") {
      assert.equal(typeof request.postDataJSON().open, "boolean");
      return route.fulfill({ status: 204 });
    }
    if (reads.has(url.pathname) && request.method() === "GET") return route.fulfill({ json: reads.get(url.pathname) });
    if (url.pathname.startsWith("/api/")) {
      evidence.unexpected.push({ path: url.pathname, method: request.method() });
      return route.fulfill({ status: 501, body: "Unexpected fixture route: " + url.pathname });
    }
    if (url.pathname === "/kimisub.js") evidence.kimiImports++;
    const filename = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = {
      ".html": "text/html", ".js": "text/javascript", ".css": "text/css",
      ".svg": "image/svg+xml", ".png": "image/png",
    }[path.extname(filename)];
    try {
      return await route.fulfill({ contentType, body: await fs.readFile(filename) });
    } catch (error) {
      evidence.assetErrors.push({ path: url.pathname, code: error.code });
      return route.fulfill({ status: 404, body: "Missing fixture asset" });
    }
  };
}

for (const engine of process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"]) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const [shape, descriptor] of Object.entries(descriptors)) {
      test(`${engine} ${lang} ${shape}: SDK subagent_pool is an ordinary editable field`, async (context) => {
        const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
        context.after(() => browser.close());
        const page = await browser.newPage({ viewport: { width: 1100, height: 800 } });
        page.setDefaultTimeout(5000);
        page.setDefaultNavigationTimeout(60000);
        const errors = [];
        const evidence = { posts: [], previews: [], unexpected: [], assetErrors: [], kimiImports: 0 };
        context.after(() => context.diagnostic(JSON.stringify({ descriptor, errors, evidence })));
        page.on("pageerror", (error) => errors.push(error.message));
        await page.route("**/*", server(lang, descriptor, evidence));
        await page.goto("http://magpie.test/");
        const row = page.locator(`.row.agent[data-id="${descriptor.id}"]`);
        await row.waitFor();
        await row.locator(".ag-link").click();
        await row.locator(".ag-exp").waitFor();
        const field = row.locator('[data-key="subagent_pool"]');
        assert.equal(await field.count(), 1, "SDK subagent_pool stays visible in the ordinary field picker");
        assert.equal(await field.locator(".v").textContent(), "Alpha");
        assert.equal(await row.locator(".ag-pool").count(), 0);
        await field.click();
        const target = page.locator("#pop #list li").filter({ hasText: "Beta" });
        const [written] = await Promise.all([page.waitForRequest("**/api/set"), target.click()]);
        assert.deepEqual(written.postDataJSON(), {
          agent: descriptor.id, field: "subagent_pool", value: options[1].value,
        }, "ordinary SDK field save keeps its exact agent, field and value");
        await page.waitForFunction(() => document.querySelector('.row.agent [data-key="subagent_pool"] .v')?.textContent === "Beta");
        assert.deepEqual(evidence.posts, [{
          agent: descriptor.id, field: "subagent_pool", value: options[1].value,
        }], "ordinary SDK field save keeps its exact agent, field and value");
        assert.equal(await row.locator(".ag-pool").count(), 0);
        assert.equal(evidence.kimiImports, 0, "SDK descriptor does not load the built-in Kimi renderer");
        assert.deepEqual(evidence.unexpected, [], "no unspecified API routes");
        assert.deepEqual(evidence.assetErrors, [], "all actual assets are available");
        assert.deepEqual(errors, [], "ordinary SDK field rendering and save have no pageerrors");
      });
    }
  }
}
