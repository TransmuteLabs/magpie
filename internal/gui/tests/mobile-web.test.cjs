// Run with Node's test runner and Playwright on the module path; see README.md.
// Phone navigation and content stay usable in both languages (#391). The
// desktop screenshots match BASE_REF within a small rendering tolerance
// (origin/main by default), but for the Agents page while BASE_REF hasn't its
// 「接入」 rows yet. Until BASE_REF has the visibility manager, its new footer
// alone is excluded; agent-bulk-visibility and agent-grip cover that footer's
// layout and controls. Its height is rounded only for the pixel comparison,
// so unchanged text below keeps the baseline's subpixel antialiasing. API
// boundaries are faked; the page's scrolling helpers are left intact.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { execFileSync } = require("node:child_process");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { reach, openPage } = require("./reader.cjs");
const { PNG } = require("playwright-core/lib/utilsBundle");
const { loadOfflineBaseline } = require("./fixtures/mobile-baseline.cjs");

const assets = path.resolve(__dirname, "../assets");
const baselineManifest = process.env.MOBILE_BASELINE_MANIFEST;
const baselineManifestSHA256 = process.env.MOBILE_BASELINE_MANIFEST_SHA256;
const offlineBaselineSelected = Object.hasOwn(process.env, "MOBILE_BASELINE_MANIFEST") || Object.hasOwn(process.env, "MOBILE_BASELINE_MANIFEST_SHA256");
let base = process.env.BASE_REF || "origin/main";
let baseline;
const at = "2026-09-30T12:00:00.000Z";
const groups = [{ name: "Claude Code", icon: "claudecode-color", calls: 123, errors: 2,
  input: 123456, output: 67890, cache_read: 543210, cache_write: 1234, reasoning: 12345, cost: 12.34, unpriced: 0 }];
const usage = { ...groups[0], path: "/test/usage.jsonl", bucket: "day", agents: groups,
  models: [{ ...groups[0], name: "a-model-with-a-long-name" }],
  series: Array.from({ length: 30 }, (_, i) => ({ ...groups[0], label: "09-" + String(i + 1).padStart(2, "0") })) };
// Populated rows are essential: an empty Agents list cannot expose clipped
// model/effort controls or make the desktop comparison cover those rows.
// Connected, so each opens on its pickers.
const modelOptions = [{ value: "claude-sonnet-4.5", label: "Claude Sonnet 4.5", icon: "claude-color", ref: "claude/claude-sonnet-4.5" },
  { value: "gpt-6.1-sol", label: "GPT-6.1-Sol", icon: "openai", ref: "openai/gpt-6.1-sol" }];
const modelField = (key, label, value = "") => ({ key, label, value, options: modelOptions });
const agentRows = [
  { id: "claude", name: "Claude Code", path: "/test/claude.json", icon: "claudecode-color", wired: true, fields: [
    modelField("model", "model", "claude-sonnet-4.5"),
    { key: "effort", label: "thinking", value: "high", options: [{ value: "high", label: "High" }] },
  ] },
  { id: "codex", name: "Codex", path: "/test/codex.toml", icon: "codex-color", wired: true, fields: [
    modelField("model", "model", "gpt-6.1-sol"),
    { key: "effort", label: "effort", value: "high", options: [{ value: "high", label: "High" }] },
    modelField("subagent", "subagents"),
    { key: "signin", label: "sign-in", value: "magpie", options: [{ value: "magpie", label: "magpie" }, { value: "chatgpt", label: "ChatGPT" }] },
  ] },
  { id: "omp", name: "omp", path: "/test/omp.json", icon: "omp", wired: true, fields: [
    modelField("model", "model", "gpt-6.1-sol"), modelField("subagent", "subagents"),
    modelField("small", "smol"), modelField("slow", "slow"),
  ] },
];
// the labels app.js groups into omp's roles square (OMP_ROLES)
const OMP_ROLES = ["subagents", "smol", "slow", "plan", "vision", "advisor"];
const views = ["agents", "providers", "gateway", "routing", "usage", "library", "plugins", "settings"];

// BASE_REF's assets, all read before a browser starts. A git show per file
// blocked the fake server for each of the ~100 the original page loads:
// under load its goto passed the 5s timeout, or its icons came after the
// screenshot (WebKit, the two OpenAI marks on Providers, 338 pixels).
async function prepareBaseline() {
  if (baseline) return;
  if (offlineBaselineSelected) {
    assert(baselineManifest && baselineManifestSHA256, "offline baseline requires both manifest path and independently pinned SHA256");
    const admitted = await loadOfflineBaseline(baselineManifest, baselineManifestSHA256);
    assert(!process.env.BASE_REF || process.env.BASE_REF === admitted.provenance.commit, "BASE_REF conflicts with the bound offline baseline");
    baseline = admitted.files;
    base = admitted.provenance.commit;
    return;
  }
  const root = path.resolve(assets, "../../.."), dir = "internal/gui/assets/";
  const commit = execFileSync("git", ["rev-parse", "--verify", "--end-of-options", `${base}^{commit}`], { cwd: root }).toString().trim();
  assert.match(commit, /^[a-f0-9]{40}$/, "baseline must resolve to one commit");
  const names = execFileSync("git", ["ls-tree", "-r", "--name-only", commit, dir], { cwd: root }).toString().split("\n").filter(Boolean);
  assert(names.length, "baseline assets are empty");
  const out = execFileSync("git", ["cat-file", "--batch"], { cwd: root, input: names.map((name) => `${commit}:${name}\n`).join(""), maxBuffer: 256 * 1024 * 1024 });
  const loaded = new Map();
  let offset = 0;
  for (const name of names) {
    const nl = out.indexOf(10, offset);
    assert(nl >= offset, "truncated baseline Git header");
    const header = /^([a-f0-9]{40}) blob (\d+)$/.exec(out.subarray(offset, nl).toString());
    assert(header, "baseline Git object is missing or not a blob");
    const size = Number(header[2]);
    assert(nl + 1 + size < out.length && out[nl + 1 + size] === 10, "truncated baseline Git blob");
    loaded.set(name.slice(dir.length), out.subarray(nl + 1, nl + 1 + size));
    offset = nl + 1 + size + 1;
  }
  assert.equal(offset, out.length, "unexpected baseline Git output");
  baseline = loaded;
}

function baselineFile(file) {
  if (!baseline?.has(file)) throw new Error(`${file} isn't in ${base}`);
  return baseline.get(file);
}

const mobileRouteOwners = new WeakMap();
let mobileClosingSequence = 0;

function mobileRouteOwner(page) {
  let owner = mobileRouteOwners.get(page);
  if (!owner) {
    owner = { closing: false, reason: `mobile fixture page closing ${++mobileClosingSequence}`, cancel: new AbortController(), waits: new Set(), routes: new Set() };
    mobileRouteOwners.set(page, owner);
  }
  return owner;
}

function mobileRouteResult(owner, promise) {
  return promise.then(value => ({ value }), error => {
    const message = error.message.split("\n", 1)[0];
    return {
      error,
      canceled: owner.closing && owner.cancel.signal.aborted &&
        (message === owner.reason || message.endsWith(": " + owner.reason)),
    };
  });
}

async function mobileQuotaReady(page) {
  const owner = mobileRouteOwner(page);
  if (owner.closing) return false;
  const wait = mobileRouteResult(owner, page.locator("#usageAgents .row.stat").first().waitFor({ state: "attached", timeout: 30000 }));
  owner.waits.add(wait);
  let abort;
  const canceled = new Promise(resolve => {
    abort = () => resolve({ closing: true });
    owner.cancel.signal.addEventListener("abort", abort, { once: true });
  });
  try {
    const result = await Promise.race([wait, canceled]);
    if (result.closing) return false;
    if (result.error) throw result.error;
    return true;
  } finally {
    owner.cancel.signal.removeEventListener("abort", abort);
  }
}

async function closeMobilePages(...pages) {
  const results = await Promise.allSettled(pages.map(async page => {
    const owner = mobileRouteOwner(page);
    owner.closing = true;
    owner.cancel.abort();
    const closeError = await page.context().close({ reason: owner.reason }).then(() => null, error => error);
    const pending = await Promise.all([...owner.waits, ...owner.routes]);
    owner.waits.clear();
    owner.routes.clear();
    if (closeError) throw closeError;
    for (const result of pending) if (result.error && !(result.canceled && page.isClosed())) throw result.error;
  }));
  for (const result of results) if (result.status === "rejected") throw result.reason;
}

// the files being served now: a picture still on its way (an icon's mask is
// fetched by the page itself, apart from any preload) isn't drawn yet
let serving = 0;

function server(lang, original = false, opened = []) {
  const handle = async route => {
    const url = new URL(route.request().url());
    const json = data => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: agentRows, profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true, calls: [], groups: [] } });
    if (url.pathname === "/api/groups") return json({ groups: [], pools: [], models: [] });
    if (url.pathname === "/api/usage/quotas") {
      // main's usage render can reveal the empty quota heading again if it
      // finishes last. Deliver the empty allowances after the totals paint,
      // so both screenshot pages compare the same completed API state.
      if (!await mobileQuotaReady(route.request().frame().page())) return;
      return json([]);
    }
    if (url.pathname === "/api/usage") return json(usage);
    if (url.pathname === "/api/gateway/trace") return json({ mine: true, now: at, seq: 0, routes: [], totals: { requests: 0, rerouted: 0, errors: 0 } });
    if (url.pathname === "/api/gateway/history") return json({ cut: false, days: [], routes: [] });
    if (url.pathname === "/api/library") return json({ dir: "/test/library", home: "/test", agents: [], instructions: { agents: [], sets: [] }, servers: [], skills: [], projects: [], foundServers: [], foundSkills: [] });
    if (url.pathname === "/api/plugins") return json({ bun: true, plugins: [], movable: [] });
    if (url.pathname === "/api/plugins/listings") return json({ listings: [] });
    if (url.pathname === "/api/plugins/npm") return json({ npm: {} });
    if (url.pathname === "/api/plugins/market") return json({ listings: [], state: { bun: true, plugins: [] } });
    if (url.pathname === "/api/library/reveal") { opened.push(route.request().postDataJSON()); return json({}); }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = url.pathname === "/" ? "index.html" : url.pathname.slice(1);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    serving++;
    try {
      let body;
      if (original) body = baselineFile(file);
      else body = await fs.readFile(path.join(assets, file));
      await route.fulfill({ body, contentType });
    } catch { await route.fulfill({ status: 404 }); } finally { serving--; }
  };
  return route => {
    const page = route.request().frame().page();
    const owner = mobileRouteOwner(page);
    if (owner.closing) return Promise.resolve();
    const promise = handle(route);
    owner.routes.add(mobileRouteResult(owner, promise));
    return promise;
  };
}

async function open(browser, lang, width, { original = false, touch = true, opened = [] } = {}) {
  const page = await browser.newPage({ viewport: { width, height: 900 }, hasTouch: touch, reducedMotion: "reduce", colorScheme: "light" });
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(60000);
  await page.addInitScript(at => {
    const RealDate = Date;
    window.Date = class extends RealDate {
      constructor(...args) { super(...(args.length ? args : [at])); }
      static now() { return new RealDate(at).getTime(); }
    };
  }, at);
  await page.route("http://magpie.test/**", server(lang, original, opened));
  await page.goto("http://magpie.test/");
  await page.locator("#nav button").first().waitFor();
  return page;
}

async function go(page, view) {
  // the page's tab, or on a phone's tab bar More and its row for a page
  // past the fifth (phonetabs.js)
  if (view === "settings") await page.locator("#prefs").click();
  await (view === "settings" ? page.locator(`#view-${view}`).waitFor() : openPage(page, view));
  if (view === "library") await page.locator("#view-library .lib-more:not(:disabled)").waitFor();
  if (view === "plugins") await page.locator("#view-plugins .pm-find").waitFor();
  if (view === "usage") {
    await page.locator("#usageAgents .row.stat").waitFor();
    await page.locator("#quotaHead").waitFor({ state: "hidden" });
  }
  await page.evaluate(() => document.fonts.ready);
  // every icon drawn: a mask's picture paints once it has come, which the
  // 800ms below doesn't promise on a loaded machine (nothing served for a
  // while, then a frame)
  for (let quiet = 0; quiet < 3; quiet = serving ? 0 : quiet + 1) await page.waitForTimeout(50);
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await page.waitForTimeout(800); // let the tab's spring and content entrance settle
}

// a screenshot once two in a row are the same: WebKit fetches a mask's
// picture first when it paints it, which a screenshot can be the first to do
// (an icon still on its way when the page was drawn), so the screenshot
// waits for what it asked for and is taken again
async function still(page) {
  let last;
  for (let i = 0; i < 10; i++) {
    const png = await page.screenshot({ animations: "disabled" });
    if (last && png.equals(last)) break;
    last = png;
    for (let quiet = 0; quiet < 3; quiet = serving ? 0 : quiet + 1) await page.waitForTimeout(50);
  }
  return last;
}

async function save(page, name) {
  if (!process.env.ARTIFACT_DIR) return;
  await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
  await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, name + ".png"), animations: "disabled" });
}

async function fits(page, view) {
  const dimensions = await page.locator(`#view-${view}`).evaluate(v => ({
    view: [v.scrollWidth, v.clientWidth], body: [document.body.scrollWidth, innerWidth],
    document: [document.documentElement.scrollWidth, innerWidth],
  }));
  for (const [name, [actual, available]] of Object.entries(dimensions)) assert(actual <= available + 1, `${view}: ${name} scrolls sideways (${actual} > ${available})`);
}

async function agentsFit(page) {
  for (const agent of agentRows) {
    const row = page.locator(`.row.agent[data-id="${agent.id}"]`);
    await row.waitFor();
    // opened, its pickers in the row; the last row opened can push this one
    // out of sight, so it is wheeled to once that one has slid open
    await page.waitForFunction(() => !document.getAnimations().length);
    await reach(page, row.locator(".ag-link"));
    await row.locator(".ag-link").click();
    await row.locator(".ag-exp").waitFor();
    const bad = await row.evaluate(row => {
      const box = row.getBoundingClientRect(), bad = [];
      for (const field of row.querySelectorAll(".field")) {
        const b = field.getBoundingClientRect();
        if (b.left < box.left - 1 || b.right > box.right + 1 || b.top < box.top - 1 || b.bottom > box.bottom + 1) bad.push("field cropped: " + field.dataset.key);
      }
      const model = row.querySelector('.field[data-key="model"] .v');
      if (!model?.textContent || model.scrollWidth > model.clientWidth + 1) bad.push("model name cropped");
      return bad;
    });
    assert.deepEqual(bad, [], agent.name + ": every control and model name must be visible");
    // omp's subagents, smol and slow roles share one square, as Claude
    // Code's tiers do (#1397, fc5dfa3c): it opens their menu
    const keys = [...new Set(agent.fields.map((f) => agent.id === "omp" && OMP_ROLES.includes(f.label) ? "tiers" : f.key))];
    for (const key of keys) {
      try {
        const field = row.locator(`.field[data-key="${key}"]`);
        await page.waitForFunction(() => !document.getAnimations().length);
        await reach(page, field);
        await field.click();
        await page.locator("#pop").waitFor();
      } catch (e) { throw new Error(`${agent.id} ${key} open: ${e.message.split("\n")[0]}`); }
      assert.equal(await page.locator("#pop").isVisible(), true, agent.name + ": " + key + " opens");
      // shut by a tap on the header's name: a picker opened low in a tall
      // row (Codex's holds its own settings) can cover the phone's tab bar,
      // and the page's lead line can be scrolled under the header's tabs
      const brand = await page.locator("header.top .brand").boundingBox();
      await page.mouse.click(brand.x + brand.width / 2, brand.y + brand.height / 2);
      try { await page.locator("#pop").waitFor({ state: "hidden" }); }
      catch (e) { throw new Error(`${agent.id} ${key} shut: ${e.message.split("\n")[0]}`); }
    }
  }
}

async function usageFits(page) {
  const bad = await page.evaluate(() => {
    const bad = [];
    for (const r of document.querySelectorAll(".row.stat")) {
      const box = r.getBoundingClientRect();
      for (const e of r.querySelectorAll(".num, .cost")) {
        const b = e.getBoundingClientRect();
        if (b.left < box.left || b.right > box.right + 1 || e.scrollWidth > e.clientWidth + 1) bad.push("tokens or cost cropped: " + e.textContent);
      }
    }
    for (const e of document.querySelectorAll("#stats small")) if (e.scrollWidth > e.clientWidth + 1) bad.push("stat explanation cropped: " + e.textContent);
    let last;
    const dates = [];
    for (const e of document.querySelectorAll("#chart .labels span")) {
      if (!e.textContent || getComputedStyle(e).visibility === "hidden") continue;
      dates.push(e.textContent);
      const text = document.createRange(); text.selectNodeContents(e);
      const b = text.getBoundingClientRect();
      if (last && b.left < last.right + 2) bad.push("date labels overlap");
      const box = e.parentElement.getBoundingClientRect();
      if (b.left < box.left - 1 || b.right > box.right + 1) bad.push("date label outside chart");
      last = b;
    }
    if (dates.length < 3 || dates[0] !== "09-01" || dates[dates.length - 1] !== "09-30") bad.push("chart must retain its endpoints and intermediate dates");
    return bad;
  });
  assert.deepEqual(bad, []);
  for (const selector of ["#usageAgents .cost", "#usageModels .cost"]) assert.match(await page.locator(selector).first().textContent(), /^\$12\.34$/);
}

async function desktopSearch(page, measure = false) {
  return page.evaluate(measure => {
    const group = document.querySelector("#addSheet .add-tools");
    if (!group) return null;
    const input = group.querySelector(".find");
    if (!input) return null;
    const style = getComputedStyle(input), placeholder = getComputedStyle(input, "::placeholder");
    const keys = ["font", "letterSpacing", "wordSpacing", "fontKerning", "fontFeatureSettings",
      "fontVariationSettings", "fontOpticalSizing", "textAutospace", "textTransform",
      "direction", "textIndent", "textShadow", "textRendering"];
    const typography = style => Object.fromEntries(keys.map(key => [key, style[key]]));
    const box = node => {
      const rect = node.getBoundingClientRect();
      return { x: rect.x, y: rect.y, width: rect.width, height: rect.height, right: rect.right };
    };
    const hit = node => {
      const rect = node.getBoundingClientRect();
      const target = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2);
      return !!target && (target === node || node.contains(target));
    };
    const insets = Object.fromEntries(["paddingLeft", "paddingRight", "paddingTop", "paddingBottom",
      "borderLeftWidth", "borderRightWidth", "borderTopWidth", "borderBottomWidth"]
      .map(key => [key, parseFloat(style[key])]));
    const fullHint = t("Find a vendor, or paste a URL and key…");
    let expectedWidth;
    if (measure) {
      const meter = document.createElement("span");
      meter.lang = input.closest("[lang]")?.lang || "";
      meter.style.cssText = "position:fixed;visibility:hidden;white-space:pre;pointer-events:none";
      for (const key of keys.slice(0, 9)) meter.style[key] = style[key];
      meter.textContent = fullHint;
      document.body.append(meter);
      try {
        expectedWidth = Math.ceil(meter.getBoundingClientRect().width + insets.paddingLeft +
          insets.paddingRight + insets.borderLeftWidth + insets.borderRightWidth) + 2;
      } finally { meter.remove(); }
    }
    return {
      fullHint, text: input.placeholder, value: input.value, active: document.activeElement === input,
      lang: input.closest("[lang]")?.lang || "", qTypography: typography(style), pseudoTypography: typography(placeholder),
      paint: { background: style.backgroundColor, color: placeholder.color, opacity: placeholder.opacity },
      insets, expectedWidth, box: box(input), hit: hit(input), inline: input.getAttribute("style"),
      hintWidth: parseFloat(input.style.getPropertyValue("--hint-w")), gap: getComputedStyle(group).columnGap,
      children: Array.from(group.children, node => ({ tag: node.tagName, text: node.textContent, box: box(node), hit: hit(node) })),
    };
  }, measure);
}

async function nativeSearchPaint(page, width, source) {
  const control = await page.evaluateHandle(width => {
    const input = document.querySelector("#addSheet .add-tools > .find");
    const clone = input.cloneNode(true);
    clone.removeAttribute("id");
    clone.removeAttribute("name");
    clone.value = "";
    for (const [key, value] of Object.entries({
      position: "fixed", left: "20px", top: "600px", width: `${width}px`,
      "min-width": "0px", "max-width": "none", flex: "none", "z-index": "2147483647", "pointer-events": "none",
    })) clone.style.setProperty(key, value, "important");
    input.parentElement.append(clone);
    return clone;
  }, width);
  try {
    assert.equal(await control.evaluate(node => document.activeElement === node), false, "search native control must be unfocused");
    const image = PNG.sync.read(await control.asElement().screenshot());
    const background = source.paint.background.match(/^rgb\((\d+), (\d+), (\d+)\)$/);
    assert(background, "search native reference requires known flat input background");
    const foreground = new Map();
    const left = Math.ceil(source.insets.borderLeftWidth + source.insets.paddingLeft);
    const right = Math.floor(image.width - source.insets.borderRightWidth - source.insets.paddingRight);
    const top = Math.ceil(source.insets.borderTopWidth + source.insets.paddingTop);
    const bottom = Math.floor(image.height - source.insets.borderBottomWidth - source.insets.paddingBottom);
    for (let y = top; y < bottom; y++) for (let x = left; x < right; x++) {
      const offset = (y * image.width + x) * 4;
      const color = Array.from(image.data.subarray(offset, offset + 3));
      if (color.some((channel, index) => Math.abs(channel - Number(background[index + 1])) > 48)) {
        foreground.set(`${x},${y}`, color);
      }
    }
    return foreground;
  } finally {
    await control.evaluate(node => node.remove());
    await control.dispose();
  }
}

async function normalizeDesktopSearch(current, original) {
  const reference = await desktopSearch(original, true), actual = await desktopSearch(current);
  assert(reference && actual, "desktop search toolbar must exist on both pages");
  assert(reference.fullHint, "desktop search full baseline hint must be nonempty");
  assert.equal(reference.text, reference.fullHint, "baseline search must show the complete localized hint");
  assert.equal(actual.text, reference.fullHint, "current search must show the complete localized hint");
  assert.equal(actual.value, "", "desktop search must be empty");
  assert.equal(actual.active, false, "desktop search must be unfocused");
  for (const key of ["lang", "qTypography", "pseudoTypography", "paint", "insets", "gap"]) {
    assert.deepEqual(actual[key], reference[key], `desktop search ${key} must retain the baseline contract`);
  }
  assert.equal(reference.children.length, 3, "baseline search toolbar must contain input and two buttons");
  assert.equal(actual.children.length, 3, "current search toolbar must contain input and two buttons");
  for (const key of ["right", "y", "height"]) {
    assert.equal(actual.box[key], reference.box[key], `desktop search ${key} must stay fixed`);
  }
  assert(actual.hit, "desktop search input hit target must be unobstructed");
  for (let index = 1; index < 3; index++) {
    const before = reference.children[index], after = actual.children[index];
    assert.equal(before.tag, "BUTTON", "baseline search neighbor must be a button");
    assert.equal(after.tag, before.tag, "current search neighbor must remain a button");
    assert.equal(after.text, before.text, "current search neighboring button must retain its label");
    assert.deepEqual(after.box, before.box, "current search neighboring button bounds must stay fixed");
    assert(after.hit, "current search neighboring button hit target must be unobstructed");
    assert(after.box.x >= (index === 1 ? actual.box.right : actual.children[index - 1].box.right),
      "current search toolbar controls must not overlap");
  }
  assert(Number.isFinite(actual.box.width) && actual.box.width > 0 && actual.box.width <= original.viewportSize().width,
    "current search width must be bounded by the desktop viewport");
  const ample = await nativeSearchPaint(original, Math.max(reference.expectedWidth, actual.box.width) + 64, reference);
  const painted = await nativeSearchPaint(original, actual.box.width, reference);
  assert(ample.size > 0 && painted.size > 0, "search native ample reference and current-width paint must be nonempty");
  assert.deepEqual(painted, ample, "search hint native tail must be complete at the actual current width");
  assert.equal(actual.box.width, reference.expectedWidth, "current search width must match the independently derived baseline typography contract");
  assert.equal(actual.hintWidth, reference.expectedWidth, "current search declared hint width must match the independent contract");
  assert.equal(actual.box.x, reference.box.right - reference.expectedWidth, "current search left origin must follow its fixed right edge");
  let changed = false;
  const restore = async () => {
    try {
      if (changed) await original.locator("#addSheet .add-tools > .find").evaluate((node, inline) => {
        if (inline === null) node.removeAttribute("style"); else node.setAttribute("style", inline);
      }, reference.inline);
    } finally {
      assert.deepEqual(await desktopSearch(current), actual, "search reference normalization must leave current completely untouched");
    }
  };
  try {
    if (reference.box.width !== reference.expectedWidth) {
      changed = true;
      await original.locator("#addSheet .add-tools > .find").evaluate((node, width) => { node.style.width = width + "px"; }, reference.expectedWidth);
      const normalized = await desktopSearch(original);
      assert.equal(normalized.box.width, reference.expectedWidth, "baseline search must use the independently derived width");
      assert.equal(normalized.box.x, reference.box.right - reference.expectedWidth, "baseline flex layout must place the approved left origin");
      for (const key of ["right", "y", "height"]) assert.equal(normalized.box[key], reference.box[key], `baseline search ${key} must remain fixed`);
      assert.deepEqual(normalized.children.slice(1), reference.children.slice(1), "baseline neighboring buttons must remain unchanged");
    }
    return restore;
  } catch (error) {
    await restore();
    throw error;
  }
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": magpie web on phones", async t => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    await prepareBaseline();
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh"]) {
      for (const width of [360, 390, 430, 820]) {
        await t.test(`${lang} ${width}: every tab and page fits`, async () => {
          const opened = [], errors = [], failures = [], page = await open(browser, lang, width, { opened });
          page.on("pageerror", e => errors.push(e.message));
          try {
            for (const view of views) {
              try {
              await go(page, view);
              await save(page, `${engine}-${lang}-${width}-${view}`);
              await fits(page, view);
              assert.equal(await page.locator(`#view-${view}`).isVisible(), true);
              if (view === "agents") await agentsFit(page);
              if (view === "usage") await usageFits(page);
              if (view === "library") {
                await Promise.all([
                  page.waitForResponse(r => new URL(r.url()).pathname === "/api/library/reveal"),
                  page.getByRole("button", { name: lang === "en" ? "Library folder" : "资源库文件夹", exact: true }).click(),
                ]);
                assert.deepEqual(opened, [{ path: "/test/library" }], "Library folder must be reachable");
              }
              } catch (e) { failures.push(view + ": " + e.message); }
            }
            assert.deepEqual(errors, []);
            assert.deepEqual(failures, []);
          } finally { await closeMobilePages(page); }
        });
      }
      await t.test(lang + ": idle scripts cannot scroll, touch momentum can, then protection returns", async () => {
        const page = await open(browser, lang, 390);
        try {
          // Settings now has separate parts. Its Usage part in a short phone
          // viewport has real content to scroll; General fits without scrolling.
          await page.setViewportSize({ width: 390, height: 600 });
          await page.goto("http://magpie.test/?view=settings&tab=usage");
          await go(page, "settings");
          await page.waitForTimeout(500);
          const view = page.locator("#view-settings");
          assert(await view.evaluate(v => v.scrollHeight > v.clientHeight + 400));
          await view.evaluate(v => { v.scrollTop = 300; });
          await page.waitForTimeout(100);
          assert.equal(await view.evaluate(v => v.scrollTop), 0, "idle code cannot move a phone page");
          await view.evaluate(async v => {
            for (const name of ["touchstart", "touchmove", "touchend"]) v.dispatchEvent(new Event(name, { bubbles: true }));
            // OS momentum outlasts the original 250ms input window; the scroll
            // events arrive 60ms apart, as they do while a finger's fling slows.
            for (let i = 1; i <= 10; i++) {
              await new Promise(r => setTimeout(r, 60));
              v.scrollTop = i * 30;
            }
          });
          await page.waitForTimeout(100);
          assert.equal(await view.evaluate(v => v.scrollTop), 300, "continuous momentum must not be pulled back");
          await page.waitForTimeout(350);
          await view.evaluate(v => { v.scrollTop = 450; });
          await page.waitForTimeout(100);
          assert.equal(await view.evaluate(v => v.scrollTop), 300, "protection returns after momentum stops");
        } finally { await closeMobilePages(page); }
      });
      for (const width of [900, 1280]) {
        await t.test(`${lang} ${width}: desktop matches ${base}`, async () => {
          const current = await open(browser, lang, width, { touch: false });
          const original = await open(browser, lang, width, { touch: false, original: true });
          try {
            for (const view of views) {
              if (view === "agents" && !baselineFile("app.js").includes("function connectPanel")) continue;
              await go(current, view); await go(original, view);
              await current.mouse.move(0, 0); await original.mouse.move(0, 0);
              // Removing hover can repaint the tab's shadow in a later frame.
              await current.waitForTimeout(350); await original.waitForTimeout(350);
              // The new management footer intentionally differs from main.
              // Compare the rest at its new position: the footer also moves
              // Profiles down. Compare the footer too once main contains it.
              // Do not skip the Agents page or increase the pixel tolerance.
              let addedFooter = view === "agents" && !baselineFile("app.js").includes("function agentsManageButton")
                ? await current.locator("#agentsManage").boundingBox() : null;
              if (addedFooter) {
                // A fractional new height changes text antialiasing below it,
                // even after translating the screenshot back. Normalize only
                // that new height; the footer's real layout is tested separately.
                await current.locator("#agentsManage").evaluate((bar, height) => {
                  bar.style.boxSizing = "border-box";
                  bar.style.height = Math.ceil(height) + "px";
                }, addedFooter.height);
                addedFooter = await current.locator("#agentsManage").boundingBox();
              }
              // Plugins' Built-in tab is new: until BASE_REF has it, the
              // page is compared with that tab taken out of its strip (the
              // sidebar's list follows the strip); builtins.test.cjs covers
              // the tab and its list.
              if (view === "plugins" && !baselineFile("plugins.js").includes("function drawBuiltin")) {
                await current.locator("#view-plugins .lib-tabs .opt").first().evaluate((b) => b.remove());
                await current.waitForTimeout(350);
              }
              // Usage's lists fold under their headings and the tokens tile's
              // label is a pop-up (#2378): until BASE_REF has them, each is
              // put back to the plain label it was, and the rest of the page
              // is compared as ever; usage-overview-fold.test.cjs covers them.
              if (view === "usage" && !baselineFile("plugins/usage/page.js").toString().includes("function mountUsageFolds")) {
                await current.evaluate(() => {
                  for (const b of document.querySelectorAll("#usagePane .row-head .fold, #stats .kpi-pick")) b.replaceWith(b.querySelector(".label, span"));
                });
                await current.waitForTimeout(350);
              }
              // Usage's lists' order picks are new (onducoder on X): until
              // BASE_REF has them, they are taken out; usage-sort.test.cjs
              // covers them.
              if (view === "usage" && !baselineFile("plugins/usage/page.js").toString().includes("function usageSortPick")) {
                await current.evaluate(() => { for (const b of document.querySelectorAll("#usagePane .row-head .us-sort, #usagePane .row-head .us-sort-gap")) b.remove(); });
                await current.waitForTimeout(350);
              }
              // Replies' Short Claude system prompt row is new: until BASE_REF
              // has it, the page is compared with the row taken out;
              // claude-lean-prompt-setting.test.cjs covers the row.
              if (view === "settings" && !baselineFile("app.js").includes("claudeLeanRow")) {
                await current.locator("#claudeLeanRow").evaluate((r) => r.remove());
                await current.waitForTimeout(350);
              }
              // The Routing page's Settings › button is new: until BASE_REF
              // has it, the page is compared with the button taken out;
              // settings-places.test.cjs covers it.
              if (view === "routing" && !baselineFile("plugins/routing/client.js").toString().includes("rtToSet")) {
                await current.locator("#rtToSet").evaluate((b) => b.remove());
                await current.waitForTimeout(350);
              }
              // So is the Usage page's, in the allowances' head.
              if (view === "usage" && !baselineFile("plugins/usage/client.js").toString().includes("usageToSet")) {
                await current.locator("#usageToSet").evaluateAll((bs) => bs.forEach((b) => b.remove()));
                await current.waitForTimeout(350);
              }
              // A setting row's line wraps whole (arculongus on Discord):
              // until BASE_REF has that, the page is compared with the lines
              // held to one line as they were; settings-lines-wrap.test.cjs
              // covers the wrapping.
              if (view === "settings" && !baselineFile("app.css").includes("arculongus")) {
                await current.addStyleTag({ content: ".row.pref .sub { white-space: nowrap; overflow-wrap: normal; }" });
                await current.waitForTimeout(350);
              }
              // The sidebar's button sits ahead of the magpie in both layouts
              // (ztf4080 on X), so the header beside the sidebar no longer
              // starts with it: until BASE_REF has it, the button, the magpie
              // and the header beside the sidebar are left out of both pictures and the rest
              // compared as ever; side-toggle.test.cjs covers where they are.
              if (!baselineFile("app.js").includes("append(btn, brand)")) {
                for (const p of [current, original]) await p.addStyleTag({ content: "#sideToggle, .brand, body.side-nav .top { visibility: hidden !important; }" });
                await current.waitForTimeout(350);
              }
              const restoreSearch = view === "providers" && baselineFile("addfloat.js").includes('window.wholeHint.c ||= document.createElement("canvas").getContext("2d")')
                ? await normalizeDesktopSearch(current, original) : async () => {};
              try {
              const before = PNG.sync.read(await still(original));
              const after = PNG.sync.read(await still(current));
              const sameSize = before.width === after.width && before.height === after.height;
              let changedPixels = 0;
              // Chromium can repaint antialiased edges differently even when
              // comparing HEAD with itself. Count a pixel once only when one
              // of its channels changes by more than 48; size changes always fail.
              const scale = before.width / width;
              if (sameSize) for (let i = 0; i < before.data.length; i += 4) {
                const row = Math.floor(i / 4 / before.width), col = (i / 4) % before.width;
                // Only the footer's own column moves: the sidebar beside the
                // list (at 1280) stays where it was.
                const shifted = addedFooter && row >= Math.floor(addedFooter.y * scale)
                  && col >= Math.floor(addedFooter.x * scale) && col < Math.ceil((addedFooter.x + addedFooter.width) * scale)
                  ? Math.round(addedFooter.height * scale) : 0;
                // Both images retain their real viewport size. The added
                // footer pushes this last slice below the current viewport.
                if (row + shifted >= after.height) continue;
                const j = i + shifted * after.width * 4;
                for (let channel = 0; channel < 4; channel++) {
                  if (Math.abs(before.data[i + channel] - after.data[j + channel]) > 48) {
                    changedPixels++;
                    break;
                  }
                }
              }
              const same = sameSize && changedPixels <= 300;
              if (!same) {
                await save(original, `${engine}-${lang}-${width}-${view}-desktop-before`);
                await save(current, `${engine}-${lang}-${width}-${view}-desktop-after`);
              }
              assert(same, `${view}: desktop screenshot changed (${sameSize ? changedPixels + " pixels differ by more than 48; limit 300" : "dimensions differ"})`);
              } finally { await restoreSearch(); }
            }
          } finally { await closeMobilePages(current, original); }
        });
      }
    }
  });
}
