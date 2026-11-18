const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(process.env.MAGPIE_PREFS_APP || path.resolve(__dirname, "../assets/app.js"), "utf8");
const copy = (value) => JSON.parse(JSON.stringify(value));
const statePosts = [
  "set", "agents/connect/omp#work", "agents/restart/claude", "agents/disconnect/codex",
  "agents/disconnect-offline/aside", "agents/keep/claude", "agents/reapply/claude",
  "agents/rescan", "agents/stage/aside", "agents/telemetry/claude",
  "settings/desktop-isolated", "settings/desktop-isolated-claude",
  "profile/save", "profile/use", "profile/delete", "profile/rename", "sync",
];
function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}
function fixture(context, enriched = false) {
  let settings = { theme: "light", lang: "en", desktopIsolated: false };
  const posts = [], reads = [], holds = new Map(), failures = new Set(), pending = new Set();
  const sandbox = {
    Object, Map, Set, WeakMap, AbortController, setTimeout, clearTimeout,
    web: false, readsSame: () => false, sameReads: new Map(), readTags: new WeakMap(),
    skelHold: async () => {}, noteAccounts: () => {}, t: (text) => text,
    renderSettings: () => {}, settingsErr: (error) => { throw error; },
    EXTRA_GLYPH: { ultracode: "" }, svg: () => ({}),
    el: () => ({ append() {}, setAttribute() {}, dataset: {} }),
    renderAgents: () => {}, saidOk: () => {}, statusErr: (error) => { throw error; },
    prefs: copy(settings), state: { settings: copy(settings) },
    fetch: async (url, options) => {
      const endpoint = url.slice(5).replace(/%23/g, "#"), writing = options.body !== undefined;
      const body = writing ? JSON.parse(options.body) : undefined;
      if (options.signal?.aborted) throw options.signal.reason;
      (writing ? posts : reads).push(writing ? { endpoint, body } : endpoint);
      if (writing && endpoint === "settings") settings = { ...settings, ...body };
      if (writing && endpoint === "settings/full-context") settings.fullContext = body.on;
      if (writing && endpoint === "settings/codex-suggestions") settings.codexSuggestions = body.model;
      if (writing && endpoint === "settings/desktop-isolated") settings.desktopIsolated = body.on;
      if (writing && endpoint === "settings/desktop-isolated-claude") settings.desktopIsolatedClaude = body.on;
      let value = ["settings", "settings/full-context", "settings/codex-suggestions"].includes(endpoint) ? copy(settings) : {
        settings: copy(settings), agents: [{ id: "claude", wired: true }],
        catalog: "new agent state", notice: "agent receipt", connected: { method: "native" },
        changed: 3, library: { problems: [] },
      };
      if (endpoint === "state?last=1") value = null;
      if (enriched && endpoint === "settings") value = { ...value, version: "fixture-version", dir: "~/fixture", codexUsers: ["fixture-user"] };
      if (endpoint === "agents/cli/claude") value = { version: "9.0", error: "update diagnostic" };
      const held = holds.get(endpoint);
      if (held) {
        held.asked.resolve();
        await new Promise((resolve, reject) => {
          const abort = () => reject(options.signal.reason);
          options.signal?.addEventListener("abort", abort, { once: true });
          held.release.promise.then(() => {
            options.signal?.removeEventListener("abort", abort);
            resolve();
          });
        });
      }
      const failed = failures.has(endpoint);
      return {
        ok: !failed, status: failed ? 500 : 200, statusText: "fixture",
        headers: { get: () => null },
        text: async () => JSON.stringify(failed ? { error: "fixture refusal: " + endpoint } : value),
      };
    },
  };
  const scope = vm.createContext(sandbox);
  const start = source.indexOf("const prefSettingsRoutes");
  const end = source.indexOf("// notJSON", start);
  const ownerStart = source.indexOf("let prefsWrites =");
  const ownerEnd = source.indexOf("// PROV_PREFS:", ownerStart);
  const toggleStart = source.indexOf("const TOGGLES =");
  const toggleEnd = source.indexOf("// oneRowPerModel:", toggleStart);
  vm.runInContext(source.slice(start, end) + "\n" + source.slice(ownerStart, ownerEnd) + "\n" +
    source.slice(toggleStart, toggleEnd), scope);
  const run = (expression) => {
    const result = vm.runInContext(expression, scope);
    if (result?.then) {
      pending.add(result);
      result.then(() => pending.delete(result), () => pending.delete(result));
    }
    return result;
  };
  const request = (endpoint, body, signal) => {
    Object.assign(sandbox, { testEndpoint: endpoint, testBody: body, testSignal: signal });
    return run("api(testEndpoint, testBody, undefined, testSignal)");
  };
  context.after(async () => {
    for (const held of holds.values()) held.release.resolve();
    await Promise.allSettled([...pending]);
  });
  return {
    posts, reads, failures, sandbox, run, request,
    current: () => copy(settings),
    hold: (endpoint) => {
      const held = { asked: deferred(), release: deferred() };
      holds.set(endpoint, held);
      return { asked: held.asked.promise, release: () => { holds.delete(endpoint); held.release.resolve(); } };
    },
    dark: async () => {
      sandbox.saved = await request("settings", { theme: "dark" });
      run("prefs = saved; state.settings = saved");
    },
  };
}

for (const endpoint of statePosts) {
  test(`late ${endpoint} state preserves accepted Settings and its original agent receipt`, { timeout: 5000 }, async (context) => {
    const f = fixture(context), held = f.hold(endpoint);
    const first = f.request(endpoint, { on: true });
    await held.asked;
    await f.dark();
    held.release();
    const result = await first;
    f.sandbox.receipt = result;
    f.run("state = receipt");
    assert.equal(f.sandbox.state.settings.theme, "dark", "late state-bearing POST must not restore light");
    assert.deepEqual(copy(result.agents), [{ id: "claude", wired: true }]);
    assert.equal(result.notice, "agent receipt");
    assert.deepEqual(copy(result.connected), { method: "native" });
    assert.equal(result.changed, 3);
    assert.deepEqual(copy(result.library), { problems: [] });
    assert.deepEqual(f.posts.map((entry) => entry.endpoint), [endpoint, "settings"], "neither POST is replayed");
    assert.deepEqual(copy(result.settings), f.current(), "authority includes this POST's own Settings effects");
  });
}

for (const endpoint of ["settings/desktop-isolated", "settings/desktop-isolated-claude"]) {
  test(`${endpoint} serial state retains its own Settings effect without an authority read`, async (context) => {
    const f = fixture(context);
    const result = await f.request(endpoint, { on: true });
    assert.deepEqual(copy(result.settings), f.current());
    assert.equal(result.settings[endpoint.endsWith("-claude") ? "desktopIsolatedClaude" : "desktopIsolated"], true);
    assert.deepEqual(f.reads, []);
    assert.equal(f.posts.length, 1);
  });
}

test("accepted state POST authority failure says saved and never replays the mutation", async (context) => {
  const f = fixture(context), held = f.hold("set");
  const first = f.request("set", { value: "model" });
  await held.asked;
  await f.dark();
  f.failures.add("settings");
  held.release();
  await assert.rejects(first, (error) => error.saved === true && error.message === "fixture refusal: settings");
  assert.equal(f.posts.length, 2);
  assert.equal(f.sandbox.prefs.theme, "dark");
});

test("refused state POST preserves its transport error without a saved receipt or replay", async (context) => {
  const f = fixture(context);
  f.failures.add("set");
  await assert.rejects(f.request("set", {}), (error) => !error.saved && error.message === "fixture refusal: set");
  assert.deepEqual(f.reads, []);
  assert.equal(f.posts.length, 1);
});

test("canceling the authority read after accepted state POST preserves signal and saved receipt", async (context) => {
  const f = fixture(context), held = f.hold("set"), aborter = new AbortController();
  const first = f.request("set", {}, aborter.signal);
  await held.asked;
  await f.dark();
  const authority = f.hold("settings");
  held.release();
  await Promise.race([authority.asked, first.then(() => assert.fail("accepted stale POST needs a cancellable authority read"))]);
  aborter.abort(new Error("owned cancellation"));
  await assert.rejects(first, (error) => error.message === "owned cancellation" && error.saved === true);
  assert.equal(f.posts.length, 2);
});

test("a delayed null cached state remains a valid no-cache answer during a Settings write", async (context) => {
  const f = fixture(context), held = f.hold("state?last=1");
  const cached = f.request("state?last=1");
  await held.asked;
  await f.dark();
  held.release();
  assert.equal(await cached, null);
  assert.equal(f.posts.length, 1);
});

test("no-write bootstrap accepts incoming state settings and fresh nonsettings fields", async (context) => {
  const f = fixture(context);
  f.sandbox.state.settings.theme = "dark";
  const result = await f.request("state");
  assert.equal(result.settings.theme, "light");
  assert.equal(result.catalog, "new agent state");
  assert.deepEqual(f.posts, []);
  assert.deepEqual(f.reads, ["state"]);
});

test("a prefs-only ordinary prefWrite is authoritative without aliasing state.settings", async (context) => {
  const f = fixture(context), held = f.hold("state");
  const first = f.request("state");
  await held.asked;
  await f.run('prefWrite(api("settings/full-context", { on: true }))');
  assert.equal(f.sandbox.prefs.fullContext, true);
  assert.equal(f.sandbox.state.settings.fullContext, undefined, "ordinary consumer only publishes prefs");
  held.release();
  const result = await first;
  assert.equal(result.settings.fullContext, true);
  assert.equal(result.catalog, "new agent state");
  assert.deepEqual(f.posts, [{ endpoint: "settings/full-context", body: { on: true } }]);
});

test("unclassified CLI result keeps its original nonsettings shape", async (context) => {
  const f = fixture(context);
  const result = await f.request("agents/cli/claude", {});
  assert.deepEqual(copy(result), { version: "9.0", error: "update diagnostic" });
  assert.deepEqual(f.reads, []);
  assert.equal(f.posts.length, 1);
});

test("RAW state settings cannot replace enriched Settings authority or erase a desktop mutation", async (context) => {
  const f = fixture(context, true);
  f.sandbox.delayedSettings = await f.request("settings", { theme: "dark" });
  const desktop = await f.request("settings/desktop-isolated", { on: true });
  assert.equal(desktop.settings.desktopIsolated, true);
  assert.equal(desktop.settings.version, undefined, "state has the RAW Shared settings contract");
  assert.deepEqual(f.reads, [], "serial state POST retains its receipt without an added authority read");
  const delivered = await f.run("prefDeliver(delayedSettings)");
  assert.equal(delivered.version, "fixture-version", "a later Settings consumer still receives enriched Settings");
  assert.equal(delivered.dir, "~/fixture");
  assert.deepEqual(copy(delivered.codexUsers), ["fixture-user"]);
  assert.equal(delivered.desktopIsolated, true, "old Settings delivery includes the newer state POST's effect");
  assert.equal(delivered.theme, "dark");
  assert.equal(f.posts.length, 2);
});

for (const timing of ["late", "settled"]) {
  test(`${timing} actual fieldToggle callback retains the accepted theme and agent receipt`, async (context) => {
    const f = fixture(context), held = f.hold("set");
    const pending = f.run('fieldToggle({ id: "claude", name: "Claude Code" }, { key: "ultracode", value: "" }).onclick({ stopPropagation() {} })');
    await held.asked;
    if (timing === "settled") { held.release(); await pending; }
    await f.dark();
    held.release();
    await pending;
    assert.equal(f.sandbox.state.settings.theme, "dark");
    assert.equal(f.sandbox.prefs.theme, "dark");
    assert.equal(f.current().theme, "dark");
    assert.equal(f.sandbox.state.catalog, "new agent state");
    assert.equal(f.sandbox.state.notice, "agent receipt");
    assert.deepEqual(f.posts, [
      { endpoint: "set", body: { agent: "claude", field: "ultracode", value: "on" } },
      { endpoint: "settings", body: { theme: "dark" } },
    ]);
  });
}

for (const timing of ["late", "settled"]) {
  test(`${timing} upstream Codex suggestions retains its model and an accepted theme`, async (context) => {
    const f = fixture(context), held = f.hold("settings/codex-suggestions");
    const pending = f.run('prefWrite(api("settings/codex-suggestions", { model: "fixture/model" }))');
    await held.asked;
    if (timing === "settled") { held.release(); await pending; }
    await f.dark();
    held.release();
    await pending;
    assert.equal(f.sandbox.prefs.theme, "dark");
    assert.equal(f.sandbox.prefs.codexSuggestions, "fixture/model");
    assert.equal(f.current().codexSuggestions, "fixture/model");
    assert.deepEqual(f.posts, [
      { endpoint: "settings/codex-suggestions", body: { model: "fixture/model" } },
      { endpoint: "settings", body: { theme: "dark" } },
    ]);
  });
}
