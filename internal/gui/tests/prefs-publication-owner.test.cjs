const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test: register } = require("node:test");
let activeTest;
const test = (name, run) => register(name, { timeout: 5000 }, async (testContext) => {
  activeTest = testContext;
  await run(testContext);
});

const source = fs.readFileSync(process.env.MAGPIE_PREFS_APP || path.resolve(__dirname, "../assets/app.js"), "utf8");
const plain = (value) => JSON.parse(JSON.stringify(value));
const initial = { theme: "light", lang: "en", contextCap: 128000, contextCapAll: true, busyLoad: 0 };
const deferred = () => {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
};

function fixture() {
  let settings = plain(initial);
  let draws = 0;
  const posts = [], reads = [], dispatches = [], holds = new Map(), errors = new Map();
  const reply = (value, status = 200) => ({
    ok: status === 200, status, statusText: "fixture",
    headers: { get: () => null },
    text: async () => JSON.stringify(value),
  });
  const sandbox = {
    web: false, readsSame: () => false, sameReads: new Map(), readTags: new WeakMap(),
    skelHold: async () => {}, noteAccounts: () => {}, t: (value) => value,
    setTimeout, clearTimeout, AbortController, Object, Map, Set, WeakMap,
    prefs: plain(initial), state: { settings: plain(initial) }, prefsKeep: (value) => value,
    applyPrefs: () => false, renderAgents: () => {}, renderSettings: () => { draws++; },
    fontList: [], window: {}, applyFeatures: () => {}, syncView: null,
    status: () => {}, statusErr: () => {}, settingsErr: () => {},
    fetch: async (url, options) => {
      const endpoint = url.slice("/api/".length);
      const body = options.body === undefined ? undefined : JSON.parse(options.body);
      dispatches.push({
        endpoint, ownerReady: vm.runInContext("typeof prefRequests !== 'undefined'", context),
        active: vm.runInContext("typeof prefRequests === 'undefined' ? 0 : prefRequests.size", context),
        wrapperBusy: vm.runInContext("prefsBusy", context),
      });
      if (options.signal?.aborted) throw options.signal.reason;
      let value;
      if (body !== undefined) {
        posts.push({ endpoint, body });
        if (endpoint === "settings/context-cap") {
          delete settings.contextCap;
          if (body.at) settings.contextCap = body.at;
        } else if (endpoint === "settings/context-cap-mode") {
          delete settings.contextCapAll;
          if (body.all) settings.contextCapAll = body.all;
        } else if (endpoint === "settings/busy-load") settings.busyLoad = body.at;
        else if (endpoint === "settings/codex-auto-reset") {
          settings.codexAutoReset = body.on ? [body.user.toLowerCase()] : [];
          settings.codexAutoResetOnly = body.on && body.when ? { [body.user.toLowerCase()]: body.when } : {};
        } else if (endpoint === "settings") settings = { ...settings, ...body };
        else if (endpoint === "agents/arrange") {
          settings.agentOrder = body.order;
          settings.agentsHidden = body.hidden;
          settings.agentsShown = body.shown;
        } else if (endpoint === "agents/here") settings.agentsHere = body.on ? [body.agent] : [];
        else if (endpoint === "usage/arrange") settings.usageOrder = body.order;
        else if (endpoint === "plugin-pages/arrange") settings.pluginPageOrder = body.order;
        value = plain(settings);
        if (endpoint === "settings/agent-telemetry") value.refused = "fixture agent refused";
        if (endpoint === "settings/mtls/issue") value.mtlsSaved = "~/Downloads/fixture";
        if (endpoint === "settings/lan") value.rekeyed = "fixture rekey refused";
        if (endpoint === "settings/port") value = { settings: value, port: { moved: ["synthetic-agent"], error: "fixture port diagnostic" } };
        if (endpoint === "settings/search-keys") value = { settings: value, added: 2, had: 1 };
        if (endpoint === "settings/https") value = { on: false, saved: "fixture cert" };
        if (endpoint === "web-page") value = { on: false, port: 45678 };
        if (endpoint === "sessions/reads") value = { conversations: false };
      } else {
        assert.equal(endpoint, "settings");
        reads.push(plain(settings));
        value = plain(settings);
      }
      const failure = errors.get(endpoint);
      if (failure) return reply({ error: failure }, 500);
      if (holds.has(endpoint)) {
        const hold = holds.get(endpoint);
        hold.requested.resolve();
        const canceled = deferred();
        const onAbort = () => canceled.resolve();
        options.signal?.addEventListener("abort", onAbort, { once: true });
        await Promise.race([hold.release.promise, canceled.promise]);
        options.signal?.removeEventListener("abort", onAbort);
        if (options.signal?.aborted) throw options.signal.reason;
      }
      return reply(value);
    },
  };
  const context = vm.createContext(sandbox);
  const start = source.includes("const prefSettingsRoutes") ? source.indexOf("const prefSettingsRoutes") : source.indexOf("async function api(");
  const end = source.indexOf("// notJSON", start);
  const ownerStart = source.indexOf("let prefsWrites =");
  const ownerEnd = source.indexOf("// PROV_PREFS:", ownerStart);
  vm.runInContext(source.slice(start, end) + "\n" + source.slice(ownerStart, ownerEnd), context);
  const saveStart = source.indexOf("let prefsBase =");
  const saveEnd = source.indexOf("// ---------- header / footer", saveStart);
  vm.runInContext(source.slice(saveStart, saveEnd), context);
  const loadStart = source.indexOf("async function loadSettings()");
  const loadEnd = source.indexOf("// Writes of the settings", loadStart);
  vm.runInContext(source.slice(loadStart, loadEnd), context);
  const pending = new Set();
  const evaluate = (text) => {
    const result = vm.runInContext(text, context);
    if (result?.then) {
      pending.add(result);
      result.then(() => pending.delete(result), () => pending.delete(result));
    }
    return result;
  };
  activeTest.after(async () => {
    for (const held of holds.values()) held.release.resolve();
    holds.clear();
    await Promise.allSettled([...pending]);
  });
  return {
    posts, reads, errors, dispatches, sandbox, evaluate,
    current: () => plain(settings),
    draws: () => draws,
    set: (next) => { settings = plain(next); },
    hold: (endpoint) => {
      const hold = { requested: deferred(), release: deferred() };
      holds.set(endpoint, hold);
      return {
        requested: hold.requested.promise,
        release: () => { holds.delete(endpoint); hold.release.resolve(); },
      };
    },
    write: (endpoint, body, wrapped = true, signal) => {
      sandbox.testEndpoint = endpoint;
      sandbox.testBody = body;
      sandbox.testSignal = signal;
      return evaluate(wrapped
        ? 'writingPrefs(api(testEndpoint, testBody, undefined, testSignal)).then((next) => { prefs = state.settings = next; return next; })'
        : 'api(testEndpoint, testBody, undefined, testSignal).then((next) => { prefs = state.settings = next; return next; })');
    },
  };
}

test("the first API write has an authority owner before fetch and before its writingPrefs wrapper", async () => {
  const f = fixture();
  await f.write("settings/context-cap", { at: 0 });
  assert.deepEqual(f.dispatches[0], {
    endpoint: "settings/context-cap", ownerReady: true, active: 1, wrapperBusy: 0,
  });
  assert.equal(f.reads.length, 0);
});

test("writingPrefs preserves arbitrary queued-promise results without treating them as Settings", async () => {
  const f = fixture();
  assert.equal(await f.evaluate("writingPrefs(Promise.resolve())"), undefined);
  assert.deepEqual(plain(await f.evaluate('writingPrefs(Promise.resolve({ on: false, values: [] }))')), { on: false, values: [] });
  assert.equal(f.evaluate("prefsBusy"), 0);
  assert.equal(f.posts.length, 0);
  assert.equal(f.reads.length, 0);
});

test("a late full-settings sibling preserves acknowledged zero and false", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const first = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  await f.write("settings/context-cap", { at: 0 });
  await f.write("settings/context-cap-mode", { all: false });
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  held.release();
  await first;
  assert.equal(f.sandbox.prefs.contextCap, undefined, "late snapshot cannot restore acknowledged cap");
  assert.equal(f.sandbox.prefs.contextCapAll, undefined, "late snapshot cannot restore acknowledged mode");
  assert.equal(f.sandbox.prefs.busyLoad, 50);
  assert.equal(f.posts.length, 3, "accepted mutations are never replayed");
});

test("serial full-settings auto-reset returns arrays, objects and cleared restrictions without authority GET", async () => {
  const f = fixture();
  const on = await f.write("settings/codex-auto-reset", { user: "Me@example.com", on: true, when: "week" });
  assert.deepEqual(plain(on.codexAutoReset), ["me@example.com"]);
  assert.deepEqual(plain(on.codexAutoResetOnly), { "me@example.com": "week" });
  const off = await f.write("settings/codex-auto-reset", { user: "Me@example.com", on: false });
  assert.deepEqual(plain(off.codexAutoReset), []);
  assert.deepEqual(plain(off.codexAutoResetOnly), {});
  assert.equal(f.reads.length, 0);
});

test("queued whole-settings publication and an independent held sibling do not deadlock or restore cap", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const first = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  await f.evaluate('savePrefs({ lang: "zh" })');
  assert.equal(f.sandbox.prefs.lang, "zh");
  await f.write("settings/context-cap", { at: 0 });
  held.release();
  await first;
  assert.equal(f.sandbox.prefs.lang, "zh");
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.evaluate("prefsBusy"), 0);
});

test("full-settings direct wallpaper publication shares authority with wrapped callers", async () => {
  const f = fixture(), held = f.hold("appearance/wallpaper");
  const first = f.write("appearance/wallpaper", { data: "synthetic" }, false);
  await held.requested;
  await f.write("settings/context-cap", { at: 0 });
  held.release();
  await first;
  assert.equal(f.sandbox.prefs.contextCap, undefined);
});

for (const endpoint of ["settings/port", "settings/search-keys"]) {
  test(`overlap preserves ${endpoint} operation envelope`, async () => {
    const f = fixture(), held = f.hold(endpoint);
    const pending = f.evaluate(`writingPrefs(api(${JSON.stringify(endpoint)}, {}))`);
    await held.requested;
    await f.write("settings/context-cap", { at: 0 });
    held.release();
    const result = plain(await pending);
    assert.ok(result.settings, "operation envelope must retain its Settings snapshot");
    assert.equal(result.settings.contextCap, undefined);
    if (endpoint.endsWith("port")) assert.deepEqual(result.port, { moved: ["synthetic-agent"], error: "fixture port diagnostic" });
    else assert.deepEqual({ added: result.added, had: result.had }, { added: 2, had: 1 });
  });
}

for (const [endpoint, expected] of [
  ["settings/https", { on: false, saved: "fixture cert" }],
  ["web-page", { on: false, port: 45678 }],
  ["sessions/reads", { conversations: false }],
]) {
  test(`${endpoint} remains its original nonsettings result`, async () => {
    const f = fixture();
    const held = f.hold("settings/context-cap");
    const pending = f.write("settings/context-cap", { at: 0 });
    await held.requested;
    const result = await f.evaluate(`writingPrefs(api(${JSON.stringify(endpoint)}, {}))`);
    assert.deepEqual(plain(result), expected);
    held.release();
    await pending;
  });
}

test("authority-read failure after success never publishes stale data or repeats a mutation", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const pending = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  await f.write("settings/context-cap", { at: 0 });
  f.errors.set("settings", "fixture authority unavailable");
  held.release();
  await assert.rejects(pending, (error) => error.saved === true && /fixture authority unavailable/.test(error.message));
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.current().contextCap, undefined);
  assert.equal(f.posts.length, 2);
  assert.equal(f.evaluate("prefsBusy"), 0);
});

test("an invalidated authority read cannot overwrite a newer acknowledged write", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const pending = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  await f.write("settings/context-cap", { at: 272000 });
  const reading = f.hold("settings");
  const before = f.reads.length;
  held.release();
  await new Promise(setImmediate);
  assert.ok(f.reads.length > before, "late full response requests authority before delivery");
  await reading.requested;
  const newer = f.write("settings/context-cap", { at: 0 });
  reading.release();
  const [olderResult] = await Promise.all([pending, newer]);
  assert.equal(olderResult.contextCap, undefined, "invalidated authority must not be delivered as an older success");
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.posts.length, 3);
});

test("direct API delivery rejects invalidated authority before any wrapper can repair it", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const pending = f.write("settings/busy-load", { at: 50 }, false);
  await held.requested;
  await f.write("settings/context-cap", { at: 272000 }, false);
  const reading = f.hold("settings");
  held.release();
  await reading.requested;
  const newer = f.write("settings/context-cap", { at: 0 }, false);
  reading.release();
  const [olderResult] = await Promise.all([pending, newer]);
  assert.equal(olderResult.contextCap, undefined, "direct API result must be current before delivery");
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.posts.length, 3);
});

test("same-key reversed delivery follows durable state rather than last-issued or last-returned snapshot", async () => {
  const f = fixture(), held = f.hold("settings/context-cap");
  const pending = f.write("settings/context-cap", { at: 272000 });
  await held.requested;
  const current = f.current();
  delete current.contextCap;
  f.set(current);
  await f.write("settings/context-cap-mode", { all: false });
  held.release();
  await pending;
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.sandbox.prefs.contextCapAll, undefined);
  assert.equal(f.posts.length, 2);
});

test("delivery rechecks a wrapped API result held before writingPrefs returns", async () => {
  const f = fixture(), gate = deferred();
  f.sandbox.deliveryGate = gate.promise;
  const pending = f.evaluate('writingPrefs(api("settings/busy-load", { at: 50 }).then(async (next) => { await deliveryGate; return next; })).then((next) => { prefs = state.settings = next; return next; })');
  await new Promise(setImmediate);
  await f.write("settings/context-cap", { at: 0 });
  gate.resolve();
  await pending;
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.posts.length, 2);
});

for (const [endpoint, key, expected] of [
  ["settings/agent-telemetry", "refused", "fixture agent refused"],
  ["settings/mtls/issue", "mtlsSaved", "~/Downloads/fixture"],
  ["settings/lan", "rekeyed", "fixture rekey refused"],
]) {
  test(`overlap retains ${key} operation side effect diagnostic`, async () => {
    const f = fixture(), held = f.hold(endpoint);
    const pending = f.write(endpoint, {});
    await held.requested;
    await f.write("settings/context-cap", { at: 0 });
    held.release();
    const result = await pending;
    assert.equal(result.contextCap, undefined);
    assert.equal(result[key], expected);
    const other = await f.write("settings/context-cap-mode", { all: false });
    assert.equal(Object.hasOwn(other, key), false, "operation diagnostic must not leak to a different result");
  });
}

test("canceling one accepted authority read releases it without canceling or replaying its sibling", async () => {
  const f = fixture(), held = f.hold("settings/busy-load"), controller = new AbortController();
  const pending = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  const reading = f.hold("settings");
  const canceled = f.write("settings/context-cap", { at: 0 }, true, controller.signal);
  await new Promise(setImmediate);
  assert.ok(f.reads.length > 0, "accepted overlap requests authority before cancellation");
  await reading.requested;
  controller.abort(new Error("fixture caller canceled"));
  await assert.rejects(canceled, (error) => error.saved === true && error.message === "fixture caller canceled");
  reading.release();
  held.release();
  await pending;
  assert.equal(f.sandbox.prefs.contextCap, undefined);
  assert.equal(f.posts.length, 2);
  assert.equal(f.evaluate("prefsBusy"), 0);
});

test("the real loadSettings fence retains a direct wallpaper write even without a writingPrefs wrapper", async () => {
  const f = fixture(), held = f.hold("appearance/wallpaper");
  const pending = f.write("appearance/wallpaper", { data: "synthetic" }, false);
  await held.requested;
  assert.equal(f.evaluate("prefsBusy"), 0);
  await f.evaluate("loadSettings()");
  assert.equal(f.draws(), 0, "an active direct settings mutation fences the ordinary settings redraw");
  held.release();
  await pending;
});

for (const [endpoint, body, key] of [
  ["agents/arrange", { order: ["codex"], hidden: [], shown: ["codex"] }, "agentOrder"],
  ["agents/here", { agent: "codex", on: true }, "agentsHere"],
  ["usage/arrange", { order: ["codex"] }, "usageOrder"],
  ["plugin-pages/arrange", { order: ["fixture"] }, "pluginPageOrder"],
]) {
  test(`a delayed blind receipt observes the typed ${endpoint} owner without adding a failing read to that owner`, async () => {
    const f = fixture(), gate = deferred();
    f.sandbox.deliveryGate = gate.promise;
    const pending = f.evaluate('writingPrefs(api("settings/busy-load", { at: 50 }).then(async (next) => { await deliveryGate; return next; })).then((next) => { prefs = state.settings = next; return next; })');
    await new Promise(setImmediate);
    f.sandbox.typedEndpoint = endpoint;
    f.sandbox.typedBody = body;
    f.sandbox.typedKey = key;
    f.errors.set("settings", "fixture authority read must not run for typed owner");
    await f.evaluate('api(typedEndpoint, typedBody).then((next) => { state.settings = { ...state.settings, [typedKey]: next[typedKey] }; })');
    assert.deepEqual(plain(f.sandbox.state.settings[key]), f.current()[key]);
    assert.equal(f.reads.length, 0, "typed owner's accepted write keeps its existing publication/error contract");
    f.errors.delete("settings");
    gate.resolve();
    await pending;
    assert.deepEqual(plain(f.sandbox.state.settings[key] ?? null), f.current()[key], "blind snapshot must not erase an acknowledged typed mutation");
    assert.equal(f.posts.length, 2);
  });
}

test("an accepted typed owner cannot send an older held full response back over its owned fields", async () => {
  const f = fixture(), held = f.hold("settings/busy-load");
  const pending = f.write("settings/busy-load", { at: 50 });
  await held.requested;
  await f.evaluate('api("agents/arrange", { order: ["codex"], hidden: [], shown: [] }).then((next) => { state.settings = { ...state.settings, agentOrder: next.agentOrder }; })');
  held.release();
  await pending;
  assert.deepEqual(plain(f.sandbox.state.settings.agentOrder ?? null), ["codex"]);
  assert.equal(f.posts.length, 2);
});

test("failed delivery authority never rolls back an already published typed owner or replays a POST", async () => {
  const f = fixture(), gate = deferred();
  f.sandbox.deliveryGate = gate.promise;
  const pending = f.evaluate('writingPrefs(api("settings/busy-load", { at: 50 }).then(async (next) => { await deliveryGate; return next; })).then((next) => { prefs = state.settings = next; return next; })');
  await new Promise(setImmediate);
  await f.evaluate('api("agents/arrange", { order: ["codex"], hidden: [], shown: [] }).then((next) => { state.settings = { ...state.settings, agentOrder: next.agentOrder }; })');
  f.errors.set("settings", "fixture delivery authority unavailable");
  gate.resolve();
  await assert.rejects(pending, (error) => error.saved === true && error.message === "fixture delivery authority unavailable");
  assert.deepEqual(plain(f.sandbox.state.settings.agentOrder), ["codex"]);
  assert.equal(f.posts.length, 2);
  assert.equal(f.evaluate("prefsBusy"), 0);
});

test("a typed POST failure retains its original error and does not read authority or replay", async () => {
  const f = fixture();
  f.errors.set("agents/arrange", "fixture arrangement rejected");
  await assert.rejects(f.evaluate('api("agents/arrange", { order: [], hidden: [], shown: [] })'), (error) =>
    !error.saved && error.message === "fixture arrangement rejected");
  assert.equal(f.reads.length, 0);
  assert.equal(f.posts.length, 1);
  assert.equal(f.evaluate("prefsBusy"), 0);
});
