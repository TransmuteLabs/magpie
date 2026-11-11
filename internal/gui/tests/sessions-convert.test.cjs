// Run with Node's test runner and Playwright on the module path; see README.md.
// Claude Code and omp sessions carried on in the other agents: Continue in
// offers both destinations, converted only when
// it is picked. Listing posts nothing; picking posts sessions/carry with
// the session's agent and id, to, and open, then copies the command the
// server made, or says it is opening in the session terminal. The details
// say a new session is made, and their copy button converts too. App menu,
// never a <select>; no click moves the page. In every language, Chromium
// and WebKit.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const at = (min) => new Date(Date.now() - min * 60e3).toISOString();
const RESUME = "cd '/work/app' && codex resume 01a17137-db85-7b7d-9a5c-2df13858b071";
const OMP_RESUME = "cd '/work/app' && omp --resume 019c625b-b900-7000-8000-000000000001";
const CLAUDE_RESUME = "cd '/work/app' && claude --resume 83bd2123-8264-4f2a-bfb0-4e2705b99f6a";
const CONVERSIONS = [
  { source: "claude", id: "c-1", first: { agent: "codex", name: "Codex", command: RESUME }, second: { agent: "omp", name: "omp", command: OMP_RESUME } },
  { source: "omp", id: "o-1", first: { agent: "claude", name: "Claude Code", command: CLAUDE_RESUME }, second: { agent: "codex", name: "Codex", command: RESUME } },
];
const sess = (agent, id, title, min, extra) => ({
  agent, id, cwd: "/work/app", title, start: at(min + 5), last: at(min), models: [], transcript: true,
  resume: `cd '/work/app' && claude --resume ${id}`, path: `~/.claude/projects/-work-app/${id}.jsonl`, size: 4096, messages: 6, files: 1, deletable: true, ...extra,
});

function serve(lang, calls, conversion) {
  const { source, id, first, second } = conversion;
  const sessions = [sess(source, id, "My dog is called Biscuit", 5, {
    carry: [first, second].map(({ agent }) => ({ agent, convert: true })),
    ...(source === "omp" ? { resume: OMP_RESUME, path: "~/.omp/agent/sessions/-work-app/session_o-1.jsonl" } : {}),
  })];
  const state = { agents: [], profiles: [], settings: { lang, theme: "light" } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/sessions/manage") {
      return json({
        agents: [
          { agent: "claude", count: source === "claude" ? 1 : 0, deletable: true, name: "Claude Code", icon: "claude" },
          { agent: "codex", count: 0, deletable: true, name: "Codex", icon: "codex" },
          { agent: "omp", count: source === "omp" ? 1 : 0, deletable: true, name: "omp", icon: "omp" },
        ],
        agent: source, sessions, terminal: true, trash: [], trashDir: "~/trash",
      });
    }
    if (url.pathname === "/api/sessions/carry") {
      const body = route.request().postDataJSON();
      calls.push({ path: "carry", body });
      const target = [first, second].find((target) => target.agent === body.to);
      assert(target, `conversion target ${body.to} was not offered`);
      return json({ agent: target.agent, id: "converted", command: target.command });
    }
    if (url.pathname === "/api/copy") {
      calls.push({ path: "copy", body: route.request().postDataJSON() });
      return json({});
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    const body = await fs.readFile(file).catch(() => null);
    await (body ? route.fulfill({ body, contentType }) : route.fulfill({ status: 404 }));
  };
}

const KEYS = ["Convert, then copy the command", "Convert, then open in session terminal", "A new {agent} session is made from this conversation",
  "Made a new {agent} session from this one, opening in session terminal", "Made a new {agent} session from this one, and copied the command that opens it"];
const words = {
  en: { nav: "Sessions", line: "Continue in Codex", made: "A new Codex session is made from this conversation", copied: "Made a new Codex session from this one, and copied the command that opens it", opening: "Made a new Codex session from this one, opening in session terminal" },
  zh: { nav: "会话", line: "用 Codex 继续", made: "将从这段对话新建一个 Codex 会话", copied: "已从此会话新建 Codex 会话，并复制了打开它的命令", opening: "已从此会话新建 Codex 会话，正在所选终端中打开" },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
    for (const conversion of CONVERSIONS) {
    test(`${engine} ${lang}: ${conversion.source} conversion destinations stay distinct in the menu and details`, async (t) => {
      const { source, id, first, second } = conversion;
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      const page = await (await browser.newContext({ viewport: { width: 900, height: 600 }, reducedMotion: "reduce" })).newPage();
      t.after(() => browser.close());
      page.setDefaultTimeout(5000);
      page.setDefaultNavigationTimeout(60000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      page.on("dialog", (d) => { errors.push("a browser dialog: " + d.message()); d.dismiss(); });
      const calls = [];
      await page.route("**/*", serve(lang, calls, conversion));
      await page.goto("http://magpie.test/?view=providers");
      const tr = (k, v) => page.evaluate(([k, v]) => t(k, v), [k, v]);
      await page.waitForFunction(() => typeof t === "function" && typeof I18N === "object");
      await page.locator("#nav").getByRole("button", { name: words[lang]?.nav || await tr("Sessions"), exact: true }).click();
      const view = page.locator("#view-sessions");
      const row = view.locator(`.row.sm-sess[data-id="${id}"]`);
      await row.waitFor();
      // the Sessions page's own words are in once it is shown
      if (lang !== "en") {
        const missing = await page.evaluate(([keys, lang]) => keys.filter((k) => !I18N[lang]?.[k] || I18N[lang][k] === k), [KEYS, lang]);
        assert.deepEqual(missing, [], `every new string has its ${lang}`);
      }
      const w = source === "claude" && words[lang] || {
        line: await tr("Continue in {agent}", { agent: first.name }), made: await tr(KEYS[2], { agent: first.name }),
        copied: await tr(KEYS[4], { agent: first.name }), opening: await tr(KEYS[3], { agent: first.name }),
      };
      await page.waitForTimeout(200);
      assert.deepEqual(calls.filter((c) => c.path === "carry"), [], "listing converts nothing");
      const top = () => row.evaluate((e) => e.getBoundingClientRect().top);
      const still = async (what, click) => {
        const before = await top();
        await click();
        await page.waitForTimeout(250);
        assert.equal(await top(), before, `${what} moved the page`);
      };
      const wait = async (n) => { for (let i = 0; i < 40 && calls.length < n; i++) await page.waitForTimeout(50); };

      const carry = row.locator(".sess-carry");
      assert.equal(await carry.count(), 1);
      await still("opening Continue in", () => carry.click());
      const menu = page.locator(".proto-menu.sess-carry-menu");
      await menu.waitFor();
      assert.deepEqual(await menu.locator(".pm-item .pm-name").allTextContents(), [first.name, first.name, second.name, second.name]);
      assert.deepEqual(await menu.locator(".pm-item .pm-note").allTextContents(), [await tr(KEYS[0]), await tr(KEYS[1]), await tr(KEYS[0]), await tr(KEYS[1])]);
      assert.equal(await menu.locator(".pm-item").first().getAttribute("title"), w.made);
      // actions, nothing ticked: no empty tick column, so each agent's logo
      // starts where the heading's words do
      const [headX, icX] = await menu.evaluate((m) => [m.querySelector(".pm-head"), m.querySelector(".pm-item .ic")].map((e) => e.getBoundingClientRect().left + parseFloat(getComputedStyle(e).paddingLeft)));
      assert(Math.abs(headX - icX) < 1, `the logo lines up with the heading (${icX} vs ${headX})`);
      assert.equal(await page.locator("select").count(), 0, "no native select");
      assert.deepEqual(calls, [], "opening the menu converts nothing");

      // copy: converted, then the server's command copied
      await still("converting and copying", () => menu.locator(".pm-item").nth(0).click());
      await wait(2);
      assert.deepEqual(calls, [
        { path: "carry", body: { agent: source, id, to: first.agent, open: false } },
        { path: "copy", body: { text: first.command } },
      ]);
      await page.locator("#status", { hasText: w.copied }).waitFor();

      // the session terminal: converted and opened by the server
      calls.length = 0;
      await carry.click();
      await menu.waitFor();
      await still("converting and opening", () => menu.locator(".pm-item").nth(1).click());
      await wait(1);
      assert.deepEqual(calls, [{ path: "carry", body: { agent: source, id, to: first.agent, open: true } }]);
      await page.locator("#status", { hasText: w.opening }).waitFor();

      // Selecting the second destination must not reuse the first one's
      // conversion or resume line, for either copy or terminal.
      calls.length = 0;
      await carry.click();
      await menu.waitFor();
      await still(`converting for ${second.name} and copying`, () => menu.locator(".pm-item").nth(2).click());
      await wait(2);
      assert.deepEqual(calls, [
        { path: "carry", body: { agent: source, id, to: second.agent, open: false } },
        { path: "copy", body: { text: second.command } },
      ]);
      await page.locator("#status", { hasText: await tr(KEYS[4], { agent: second.name }) }).waitFor();
      calls.length = 0;
      await carry.click();
      await menu.waitFor();
      await still(`converting for ${second.name} and opening`, () => menu.locator(".pm-item").nth(3).click());
      await wait(1);
      assert.deepEqual(calls, [{ path: "carry", body: { agent: source, id, to: second.agent, open: true } }]);
      await page.locator("#status", { hasText: await tr(KEYS[3], { agent: second.name }) }).waitFor();

      // the details say a new session is made, and their button converts
      calls.length = 0;
      await still("opening the row", () => row.locator(".who").click());
      const detail = view.locator(".sess-detail");
      await detail.waitFor();
      const line = detail.locator(".sess-line", { has: page.locator(".k", { hasText: w.line }) });
      assert.equal(await line.count(), 1, `the details have ${w.line}`);
      assert((await line.textContent()).includes(w.made));
      assert.deepEqual(calls, [], "opening the details converts nothing");
      await still("the details' copy", () => line.locator("button.copy").click());
      await wait(2);
      assert.deepEqual(calls.map((c) => c.path), ["carry", "copy"]);
      calls.length = 0;
      const secondLine = detail.locator(".sess-line", { has: page.locator(".k", { hasText: await tr("Continue in {agent}", { agent: second.name }) }) });
      assert.equal(await secondLine.count(), 1, `${second.name} is available independently in the details`);
      assert((await secondLine.textContent()).includes(await tr(KEYS[2], { agent: second.name })));
      await still(`the details' ${second.name} copy`, () => secondLine.locator("button.copy").click());
      await wait(2);
      assert.deepEqual(calls, [
        { path: "carry", body: { agent: source, id, to: second.agent, open: false } },
        { path: "copy", body: { text: second.command } },
      ]);
      assert.deepEqual(errors, []);
    });
    }
  }
}
