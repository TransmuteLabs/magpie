const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const assets = path.join(__dirname, "../assets");
const plugins = fs.readFileSync(path.join(assets, "plugins.js"), "utf8");
const app = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const options = fs.readFileSync(path.join(assets, "plugininstalled.js"), "utf8");
const signIn = app.slice(app.indexOf("async function pluginSignIn("), app.indexOf("// openProvider:"));
const owner = (name) => {
  const at = app.indexOf("const " + name + " =");
  assert.ok(at >= 0);
  return app.slice(at, app.indexOf("\n", at));
};

class Element {
  children = [];
  attributes = {};
  classList = { add: (value) => { this.className += " " + value; } };
  constructor(tag, className = "", text = "") {
    Object.assign(this, { tag, className, text });
  }
  append(...children) { this.children.push(...children); }
  get textContent() { return this.text + this.children.map((child) => child.textContent ?? child).join(""); }
  set textContent(value) { this.text = value; this.children = []; }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener() {}
  all(match) {
    return [...(match(this) ? [this] : []), ...this.children.flatMap((child) => child.all?.(match) || [])];
  }
}

function fixture(last = false) {
  const calls = [];
  let menu;
  const context = vm.createContext({
    view: "agents", SUBS: [], panelAccts: null,
    providers: {
      last,
      providers: [{ account: { agent: "exact-sdk-go", logins: [{ extensionGeneration: "1" }] } }],
      sdkSubscriptions: [
        { id: "exact-sdk-zen", name: "Example Zen", revision: "native-fixture", methods: [{ id: "key" }] },
        { id: "exact-sdk-public", name: "Example Public", page: "sdk-fixture" },
        { id: "unrelated-sdk", name: "Example Zen", revision: "unrelated" },
      ],
      plugins: [{ id: "legacy-vendor", name: "Legacy vendor", spec: "@fixture/legacy" }],
    },
    $: () => new Element("div"), el: (...args) => new Element(...args),
    document: { createTextNode: (text) => new Element("text", "", text) },
    localStorage: { getItem: () => null },
    t: (text, fields) => Object.entries(fields || {}).reduce((out, [key, value]) => out.replaceAll(`{${key}}`, value), text),
    svg: () => new Element("svg"), icon: (name) => Object.assign(new Element("span", "ic"), { icon: name }),
    markdown() {}, mdLoad() {},
    openProtoMenu(_button, choices, _value, select) { menu = { choices, select }; },
    scrollOnPurpose: () => false,
    show: async (view) => { calls.push(["show", view]); return true; },
    adding: false, editing: null, draft: null, presetQuery: "",
    closeModal() {}, renderProviders() {},
    startSignIn(id) { calls.push(["start", id]); },
  });
  context.window = context;
  const source = plugins.replace(/\}\)\(\);\s*$/, "window.installedRow = installedRow;\n})();");
  assert.notEqual(source, plugins);
  vm.runInContext(`${owner("PUZZLE")}\n${owner("hasLogins")}\n${owner("sdkSubs")}\n${signIn}\n${options}\n${source}`, context);
  return { row: context.installedRow, calls, menu: () => menu, providers: context.providers };
}

const suite = {
  spec: "sdk:list-suite", ownName: "Fixture suite", icon: "opencode", version: "0.8.3",
  isAgent: true, providers: ["Example Go", "Example Zen", "Example Public", "Unconfigured"], moved: [],
  serves: ["Example Go", "Example Zen", "Example Public", "Unconfigured"],
  panels: [{ slot: "sessions", title: "Fixture history" }],
  sdk: {
    api: "1.12", runtime: "native", capabilities: ["account-auth", "provider-signin"],
    providerIDs: ["exact-sdk-go", "exact-sdk-zen", "exact-sdk-public", "exact-sdk-private"],
    settings: { schema: {}, values: {} },
  },
};

const buttons = (row) => row.all((node) => node.tag === "button").map((button) => button.textContent);

test("Installed SDK row uses ordinary provider accounts and authorized sign-in choices", async () => {
  const { row, calls, menu } = fixture();
  const installed = row(suite);
  assert.match(installed.textContent, /Fixture suite.*Serves Example Go, Example Zen, Example Public, Unconfigured/);
  assert.equal(installed.all((node) => node.icon)[0].icon, "opencode");
  assert.match(installed.textContent, /Example Go: signed in/);
  assert.match(installed.textContent, /Example Zen: not signed in/);
  assert.match(installed.textContent, /Example Public: not signed in/);
  assert.doesNotMatch(installed.textContent, /Unconfigured: not signed in/);
  assert.match(installed.textContent, /Adds panels to Sessions/);
  assert.match(installed.textContent, /SDK 1.12 · native/);
  assert.ok(buttons(installed).includes("Options"));
  const sign = installed.all((node) => node.tag === "button" && node.text === "Sign in to another…")[0];
  assert.ok(sign, "the other subscriptions need a Sign in button");
  const event = { stopPropagation() {} };
  sign.onclick(event);
  assert.deepEqual(Array.from(menu().choices, (choice) => choice.v), ["exact-sdk-zen", "exact-sdk-public"]);
  await menu().select("exact-sdk-zen", event);
  assert.deepEqual(calls, [["show", "providers"], ["start", "exact-sdk-zen"]]);
});

test("Installed offers sign-in for expired SDK accounts and keeps paused accounts signed in", async () => {
  const { row, calls, providers } = fixture();
  const id = "exact-sdk-go";
  const entry = { ...suite, providers: ["Example Go"], serves: ["Example Go"], sdk: { ...suite.sdk, providerIDs: [id] } };
  providers.sdkSubscriptions.push({ id, revision: "native-fixture", methods: [{ id: "key" }] });
  const logins = providers.providers[0].account.logins;
  logins[0].lapsed = "Sign-in has expired";
  const expired = row(entry);
  assert.match(expired.textContent, /Example Go: not signed in/);
  const sign = expired.all((node) => node.tag === "button" && node.text === "Sign in")[0];
  assert.ok(sign);
  sign.onclick({ stopPropagation() {} });
  await new Promise(setImmediate);
  assert.deepEqual(calls, [["show", "providers"], ["start", id]]);

  logins.push({ extensionGeneration: "2", paused: true });
  const paused = row(entry);
  assert.match(paused.textContent, /Example Go: signed in/);
  assert.ok(!buttons(paused).includes("Sign in"));
});

test("Installed rows suppress unavailable SDK actions and preserve legacy sign-in", async () => {
  const { row, calls } = fixture();
  const off = row({ ...suite, off: true });
  assert.ok(buttons(off).includes("Switch on"));
  assert.match(off.textContent, /Off.*SDK 1.12 · native/);
  assert.doesNotMatch(off.textContent, /not signed in|Serves|Options|Sign in/);

  const readOnly = row({ ...suite, sdk: { ...suite.sdk, settings: null, capabilities: ["outbound-executor"] } });
  assert.doesNotMatch(readOnly.textContent, /signed in|Options|Sign in/);
  const noGrant = row({ ...suite, providers: ["Unconfigured"], serves: [], sdk: { ...suite.sdk, settings: null, providerIDs: ["exact-sdk-private"] } });
  assert.doesNotMatch(noGrant.textContent, /signed in|Options|Sign in/);
  assert.doesNotMatch(fixture(true).row(suite).textContent, /signed in|Sign in/);

  const legacy = row({ spec: "@fixture/legacy", providers: ["Legacy vendor"], moved: [] });
  assert.match(legacy.textContent, /Legacy vendor: not signed in/);
  const sign = legacy.all((node) => node.tag === "button" && node.text === "Sign in")[0];
  assert.ok(sign, "legacy sign-in is still offered");
  sign.onclick({ stopPropagation() {} });
  await new Promise(setImmediate);
  assert.deepEqual(calls, [["show", "providers"], ["start", "legacy-vendor"]]);
});
