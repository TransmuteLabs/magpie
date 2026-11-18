const assert = require("node:assert/strict");
const { readFile } = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const vm = require("node:vm");

const directory = path.resolve(__dirname, "..");
const selected = { source: "history", viewRef: "view", rootRef: "root" };

async function fixture(options = {}) {
  const calls = [], prompts = [], listeners = new Map(), messages = [];
  const abort = new AbortController();
  let inspection = { module: "module", source: "history", agent: "native", viewRef: "view",
    ownOff: false, readAllowed: false, roots: [{ slot: "data", rootRef: "root", available: true, consented: false }],
    replaces: ["previous / history"] };
  const owner = { entry: { ref: "issued-by-parent", reads: [] }, shown: true, abort };
  const context = vm.createContext({
    Uint8Array, ArrayBuffer, URL, Blob, setTimeout, clearTimeout,
    async api(route, body) {
      calls.push({ route, body: structuredClone(body) });
      if (options.hold) await options.hold(body);
      if (body.operation === "hello") return { features: options.unsupported ? [] : ["session-history-v1"] };
      if (body.operation === "inspect") return structuredClone(inspection);
      if (body.operation === "request") {
        if (options.access) return options.access(inspection);
        inspection = { ...inspection, readAllowed: true,
          roots: [{ ...inspection.roots[0], consented: true, consentRef: "consent" }] };
        return { state: "ready", inspection: structuredClone(inspection) };
      }
      if (body.operation === "release") return { state: "released", inspection: structuredClone(inspection) };
      if (body.operation === "list") return { entries: [{ sessionRef: "session", title: "<script>inert</script>" }] };
      if (body.operation === "read") return { sessionRef: "session", readRef: "read", parts: [], cut: false, media: [] };
      throw new Error("unexpected owner operation");
    },
    confirmAction(...args) { prompts.push(args); return options.confirm ? options.confirm(...args) : Promise.resolve(true); },
    async fetch(url, request) {
      calls.push({ url, body: JSON.parse(request.body) });
      assert.equal(url, "/api/plugin-pages/history");
      assert.equal(request.credentials, "same-origin");
      return new Response(options.media ?? Uint8Array.from([1, 2, 3]), {
        headers: { "Content-Type": "image/png", ...(options.headers ?? {}) },
      });
    },
  });
  vm.runInContext(await readFile(path.join(directory, "assets/i18n.js"), "utf8"), context);
  vm.runInContext(`locale = ${JSON.stringify(options.lang || "en")};`, context);
  const source = await readFile(path.join(directory, "assets/extensionwindows.js"), "utf8");
  vm.runInContext(source.replace(/^export /gm, "") + "\nthis.answer = answerPluginPage;", context);
  const theme = () => ({ dark: false, lang: options.lang || "en", tokens: {} });
  const answer = (method, params) => context.answer(owner, { id: calls.length + 1, method, params }, theme, () => "Module");
  const bridgeParent = { postMessage(message) {
    messages.push(structuredClone(message));
    Promise.resolve(answer(message.method, message.params)).then(
      result => deliver({ magpie: 1, id: message.id, result }),
      error => deliver({ magpie: 1, id: message.id, error: error.message }),
    );
  } };
  const deliver = data => { for (const listener of listeners.get("message") ?? []) listener({ source: bridgeParent, data }); };
  const bridgeContext = vm.createContext({
    window: {}, parent: bridgeParent, Blob, URL, setTimeout, clearTimeout,
    addEventListener(name, listener) { listeners.set(name, [...(listeners.get(name) ?? []), listener]); },
    location: { href: "https://magpie.invalid/_magpie/plugin/issued/index.html" },
    document: { readyState: "loading", documentElement: { dataset: {}, style: { setProperty() {} } },
      createElement() { return { dataset: {} }; }, head: { prepend() {} } },
  });
  vm.runInContext(await readFile(path.join(directory, "pagebridge.js"), "utf8"), bridgeContext);
  await bridgeContext.window.magpie.ready;
  return { calls, prompts, answer, owner, abort, messages, bridge: bridgeContext.window.magpie, deliver,
    setInspection(value) { inspection = value; } };
}

test("history feature requires explicit hello and a current declared SDK window", async () => {
  const current = await fixture();
  assert.equal(current.calls.length, 0);
  await assert.rejects(current.answer("history.inspect", { source: "history" }), /unavailable/);
  assert.equal(await current.bridge.history.ready, true);
  assert.equal(current.calls[0].body.operation, "hello");
  assert.equal((await current.bridge.history.inspect("history")).source, "history");
  assert.equal(current.calls.at(-1).body.ref, "issued-by-parent");
  const old = await fixture({ unsupported: true });
  assert.equal(await old.bridge.history.ready, false);
  await assert.rejects(old.bridge.history.inspect("history"), /unsupported/);
  assert.equal(old.calls.length, 1);
});

test("trusted parent explains both gates and replacement; iframe cannot assert consent", async () => {
  const current = await fixture();
  await current.bridge.history.ready;
  const result = await current.bridge.history.requestAccess(selected);
  assert.equal(result.state, "ready");
  assert.equal(current.prompts.length, 1);
  assert.match(current.prompts[0][1], /package and configuration/);
  assert.match(current.prompts[0][1], /allow Magpie to read this native agent/);
  assert.match(current.prompts[0][1], /previous \/ history/);
  const body = current.calls.find(call => call.body?.operation === "request").body;
  assert.deepEqual(body, { ref: "issued-by-parent", operation: "request", ...selected });
  for (const foreign of [{ confirmed: true }, { all: true }, { account: "selected" }, { path: "/foreign" }, { module: "foreign" }, { ref: "forged" }]) {
    await assert.rejects(current.answer("history.request", { ...selected, ...foreign }), /Invalid history selection/);
  }
  assert.equal(current.prompts.length, 1);
});

for (const [lang, title, proceed, delegation, reading, replacement] of [
  ["zh", "会话", "继续", "委托给此软件包", "读取此原生代理的会话", "替换当前委托：previous / history。"],
  ["zh-TW", "會話", "繼續", "委派給此套件", "讀取此原生代理程式的工作階段", "取代目前的委派：previous / history。"],
  ["ja", "セッション", "続行", "このパッケージと設定に委任", "セッションを Magpie が読み取る", "現在の委任を置き換えます：previous / history。"],
  ["de", "Sitzungen", "Weiter", "an dieses Paket und diese Konfiguration delegieren", "Sitzungen dieses nativen Agenten zu lesen", "Aktuelle Delegierung ersetzen: previous / history."],
]) {
  test(`history consent uses the shipped ${lang} translation and preserves owner identifiers`, async () => {
    const current = await fixture({ lang });
    await current.bridge.history.ready;
    assert.equal((await current.bridge.history.requestAccess(selected)).state, "ready");
    const [heading, message, button] = current.prompts[0];
    assert.equal(heading, title);
    assert.equal(button, proceed);
    assert(message.startsWith("module / history\nnative / data\n"));
    assert(message.includes(delegation));
    assert(message.includes(reading));
    assert(message.endsWith(replacement));
    assert(!message.includes("{delegations}"));
    assert.deepEqual(current.calls.find(call => call.body?.operation === "request").body,
      { ref: "issued-by-parent", operation: "request", ...selected });
  });
}

test("cancel, hidden frame and disposal cannot continue a trusted pending access request", async () => {
  const canceled = await fixture({ confirm: () => Promise.resolve(false) });
  await canceled.bridge.history.ready;
  assert.equal((await canceled.bridge.history.requestAccess(selected)).state, "canceled");
  assert(!canceled.calls.some(call => call.body?.operation === "request"));
  for (const change of ["abort", "hidden", "entry"]) {
    let release;
    const confirmed = new Promise(resolve => { release = resolve; });
    const current = await fixture({ confirm: () => confirmed });
    await current.bridge.history.ready;
    const pending = current.bridge.history.requestAccess(selected);
    while (!current.prompts.length) await new Promise(resolve => setTimeout(resolve, 0));
    if (change === "abort") current.abort.abort();
    if (change === "hidden") current.owner.shown = false;
    if (change === "entry") current.owner.entry = { ref: "different" };
    release(true);
    await assert.rejects(pending, /changed/);
    assert(!current.calls.some(call => call.body?.operation === "request"));
  }
});

test("two-step failure preserves actual returned gate status and cannot be relabeled ready", async () => {
  const current = await fixture({ access: inspection => ({ state: "changed", inspection: {
    ...inspection, readAllowed: false, roots: [{ ...inspection.roots[0], consented: true, consentRef: "bound" }],
  } }) });
  await current.bridge.history.ready;
  const result = await current.bridge.history.requestAccess(selected);
  assert.equal(result.state, "changed");
  assert.equal(result.inspection.readAllowed, false);
  assert.equal(result.inspection.roots[0].consented, true);
});

test("selected binary media uses parent authenticated bounded fetch; dispose rejects pending calls", async () => {
  const current = await fixture();
  await current.bridge.history.ready;
  const media = await current.bridge.history.media({ source: "history", viewRef: "view", sessionRef: "session", readRef: "read", mediaRef: "media" });
  assert.deepEqual([...new Uint8Array(media.data)], [1, 2, 3]);
  assert.equal(media.mime, "image/png");
  assert.equal(current.calls.at(-1).body.ref, "issued-by-parent");
  for (const options of [{ headers: { "Content-Length": String((8 << 20) + 1) } }, { media: new Uint8Array((8 << 20) + 1) }]) {
    const oversized = await fixture(options);
    await oversized.bridge.history.ready;
    await assert.rejects(oversized.bridge.history.media({ source: "history", viewRef: "view", sessionRef: "session", readRef: "read", mediaRef: "media" }), /unavailable|budget/);
  }
  current.deliver({ magpie: 1, event: "dispose" });
  await assert.rejects(current.bridge.history.read({ source: "history", viewRef: "view", sessionRef: "session" }), /disposed/);
});


test("history rechecks visibility before a delayed inspection can prompt", async () => {
  let release, inspected;
  const waiting = new Promise(resolve => { inspected = resolve; });
  const held = new Promise(resolve => { release = resolve; });
  const current = await fixture({ hold: body => {
    if (body.operation === "inspect") { inspected(); return held; }
  } });
  await current.bridge.history.ready;
  const pending = current.bridge.history.requestAccess(selected);
  await waiting;
  current.owner.shown = false;
  release();
  await assert.rejects(pending, /changed|unavailable/);
  assert.equal(current.prompts.length, 0, "a hidden frame must not open a consent prompt");
});

test("cancelled consent reinspection cannot publish after the frame hides", async () => {
  let release, inspected, count = 0;
  const waiting = new Promise(resolve => { inspected = resolve; });
  const held = new Promise(resolve => { release = resolve; });
  const current = await fixture({ confirm: async () => false, hold: body => {
    if (body.operation === "inspect" && ++count === 2) { inspected(); return held; }
  } });
  await current.bridge.history.ready;
  const pending = current.bridge.history.requestAccess(selected);
  await waiting;
  current.owner.shown = false;
  release();
  await assert.rejects(pending, /changed|unavailable/);
});

test("a hidden and reopened frame cannot publish an older JSON or media reply", async () => {
  for (const operation of ["list", "read", "media"]) {
    let release, started;
    const waiting = new Promise(resolve => { started = resolve; });
    const held = new Promise(resolve => { release = resolve; });
    const options = operation === "media" ? { media: new ReadableStream({ start(controller) {
      release = () => { controller.enqueue(Uint8Array.of(1)); controller.close(); };
    }, pull() { started(); } }) } : { hold: body => {
      if (body.operation === operation) { started(); return held; }
    } };
    const current = await fixture(options);
    await current.bridge.history.ready;
    current.owner.visibilityVersion = 1;
    const pending = current.answer("history." + operation, { source: "history", viewRef: "view", ...(operation === "list" ? { rootRef: "root" } : { sessionRef: "session", ...(operation === "media" ? { readRef: "read", mediaRef: "media" } : {}) }) });
    await waiting;
    current.owner.visibilityVersion += 2;
    release();
    await assert.rejects(pending, /changed/, operation + " must be invalidated by a visibility transition");
  }
});


test("delayed history negotiation reports the current visibility", async () => {
  let release, started;
  const waiting = new Promise(resolve => { started = resolve; });
  const held = new Promise(resolve => { release = resolve; });
  const current = await fixture({ hold: body => {
    if (body.operation === "hello") { started(); return held; }
  } });
  const pending = current.answer("hello", { features: ["session-history-v1"] });
  await waiting;
  current.owner.shown = false;
  release();
  assert.equal((await pending).visible, false, "hello must not publish visibility captured before negotiation");
});
