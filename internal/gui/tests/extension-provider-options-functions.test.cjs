const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const assets = process.env.MAGPIE_PROVIDER_OPTIONS_ASSETS || path.join(__dirname, "../assets");
const app = fs.readFileSync(path.join(assets, "app.js"), "utf8");
const renderer = fs.readFileSync(path.join(assets, "provideroptions.js"), "utf8");
const start = app.indexOf("function pluginOptionsField(");
const end = app.indexOf("// priceRateOfDraft:", start);
const actionStart = app.indexOf("async function providerAction(");
const actionEnd = app.indexOf("// saving shows", actionStart);
const dirtyStart = app.indexOf("let providerDraftRef = ");
const dirtyEnd = app.indexOf("// backdropClick", dirtyStart);
const editorStart = app.indexOf("function renderEditor(");
const editorEnd = app.indexOf("function drawEditor(", editorStart);
const reopenStart = app.indexOf("row.onclick = async () => {\n      if (!await leaveDraft())");
const reopenEnd = app.indexOf("}; // the preset sheet", reopenStart);
const reopenHelpersStart = app.indexOf("function reEdit(");
const reopenHelpersEnd = app.indexOf("async function cancelEdit(", reopenHelpersStart);
assert.ok(start >= 0 && end > start && actionStart >= 0 && actionEnd > actionStart &&
  dirtyStart >= 0 && dirtyEnd > dirtyStart && editorStart >= 0 && editorEnd > editorStart &&
  reopenStart >= 0 && reopenEnd > reopenStart && reopenHelpersStart >= 0 && reopenHelpersEnd > reopenHelpersStart);

function element(tag, className, textContent = "") {
  const node = {
    tag, className, textContent, children: [], dataset: {},
    classList: { toggle() {} },
    querySelector() { return null; },
    append(...children) {
      for (const child of children) {
        child.parent = this;
        this.children.push(child);
      }
    },
    replaceChildren(...children) {
      this.children = [];
      this.append(...children);
    },
    replaceWith(next) {
      const index = this.parent.children.indexOf(this);
      this.parent.children[index] = next;
      next.parent = this.parent;
    },
  };
  return node;
}

function harness(options = {}) {
  const calls = [], prompts = [], errors = [];
  const modal = element("div", "modal");
  modal.classList.contains = () => false;
  modal.contains = child => modal.children.includes(child);
  let loaded = false;
  const context = vm.createContext({
    draft: {}, locale: options.locale || "en", prefs: {}, editing: "provider",
    providers: { providers: [] }, state: {},
    el: element, field: (_label, box) => [box],
    $: selector => selector === "#modal" ? modal : null,
    t: (text, fields) => Object.entries(fields || {}).reduce((value, [key, replacement]) =>
      value.replaceAll("{" + key + "}", replacement), text),
    segs: (_choices, selected, changed) => {
      const control = element("control", "", selected);
      control.select = async value => { control.textContent = value; await changed(value); };
      return control;
    },
    confirmAction: async (...args) => {
      prompts.push(args);
      return options.confirm ? options.confirm(...args) : true;
    },
    api: async (route, input) => {
      calls.push({ route, input: input === undefined ? undefined : structuredClone(input) });
      return options.api ? options.api(route, input) : { version: "1" };
    },
    statusErr: error => { throw error; },
    writingPrefs: value => value,
    editorError: error => { errors.push(error); return true; },
    document: { querySelectorAll: () => [], activeElement: { matches: () => false } },
    keptIcons: {},
    headersOf: value => value,
    modelPrefsOfDraft: () => undefined,
    accountModelsDraft: () => null,
    dropSigningIn() {}, dropSDKSignIn() {}, renderProviders() {}, renderAgents() {}, saidMoved() {},
    doneSaving() {},
    structuredClone,
  });
  context.window = context;
  context.loadProviderOptions = async () => {
    if (!loaded) {
      vm.runInContext(renderer, context);
      loaded = true;
    }
  };
  const previewOwner = {};
  context.loadProviderPreview = async () => ({ providerPreview: () => previewOwner });
  vm.runInContext(
    app.slice(start, end).replaceAll('import("./provideroptions.js")', "loadProviderOptions()") +
      app.slice(actionStart, actionEnd) + app.slice(dirtyStart, dirtyEnd) + app.slice(reopenHelpersStart, reopenHelpersEnd) +
      app.slice(editorStart, editorEnd).replace('import("./providerpreview.js")', "loadProviderPreview()"), context,
  );
  context.drawEditor = current => {
    const [editor] = context.extensionOptionsField(current);
    modal.replaceChildren(editor);
    return editor;
  };
  return {
    context, calls, prompts, errors, previewOwner,
    async draw(provider) {
      const [box] = context.extensionOptionsField(provider);
      await Promise.resolve();
      await Promise.resolve();
      return box;
    },
    async legacy(sub, top) {
      const [box] = context.pluginOptionsField(sub, top);
      await Promise.resolve();
      await Promise.resolve();
      return box;
    },
    save: provider => context.savePluginOptions(null, provider),
    reopen: async (current, fresh) => {
      let reads = 0;
      context.providers = { providers: [current] };
      context.confirmDiscard = async () => true;
      context.loadProviders = async () => {
        reads++;
        context.providers = { providers: [fresh] };
      };
      const opener = vm.runInContext(
        `(p, open) => (${app.slice(reopenStart + "row.onclick = ".length, reopenEnd + 1)})`, context,
      );
      await opener(current, false)();
      return reads;
    },
  };
}

function provider(overrides = {}) {
  return {
    id: "sdk-options.fixture-vendor", name: "Vendor",
    extensionOptions: {
      module: "options.fixture", provider: "vendor", packageRevision: "revision-a",
      configVersion: "0",
      options: [{ key: "paid", label: "Paid", confirm: "Uses credits", on: false }],
      ...overrides,
    },
  };
}

const control = (box, index = 0) => box.children[index].children[1];

test("legacy switches preserve placement, explicit false and delayed saves under their own provider", async () => {
  const view = harness(), sub = { plugin: {
    pid: "legacy-own", name: "Legacy",
    options: [
      { key: "cache", label: "<b>Cache</b>", on: true, placement: "top" },
      { key: "free", label: "Free", on: false },
    ],
  } };
  const top = await view.legacy(sub, true), models = await view.legacy(sub, false);
  assert.equal(top.children.length, 1);
  assert.equal(models.children.length, 1);
  assert.equal(top.children[0].children[0].children[0].textContent, "<b>Cache</b>");
  await control(top).select("off");
  await control(models).select("on");
  await control(models).select("off");
  assert.equal(view.calls.length, 0);
  await view.context.savePluginOptions(sub, {});
  assert.deepEqual(view.calls, [{
    route: "settings/plugin-option", input: { provider: "legacy-own", key: "cache", on: false },
  }]);
});

test("opening the ordinary editor remains clean after its lazy option renderer completes", async () => {
  const view = harness();
  const box = view.context.renderEditor(provider());
  assert.equal(view.context.providerDirty(), false);
  assert.equal(await box.previewReady, view.previewOwner);
  assert.equal(view.context.providerDirty(), false, "lazy rendering must not create an unsaved edit");
  await control(box).select("on");
  assert.equal(view.context.providerDirty(), true);
});

test("external package or config redraw cannot transfer a paid acknowledgement to a new owner", async () => {
  for (const changed of [{ packageRevision: "revision-b" }, { configVersion: "1" }, { provider: "other" }, { ownerRevision: "owner-b" }]) {
    const view = harness(), original = provider({ ownerRevision: "owner-a" });
    await control(await view.draw(original)).select("on");
    const latest = provider({ ownerRevision: "owner-a", ...changed });
    await view.draw(latest);
    await assert.rejects(view.save(latest), /changed/i);
    assert.equal(view.calls.length, 0, "stale acknowledged edit must not reach configuration CAS");
    assert.equal(view.context.draft.extensionOptions.paid, true, "unsaved intent must remain inspectable");
  }
});

test("confirmation declared only in another locale is asked and reaches the save acknowledgement", async () => {
  let accepted = false;
  const view = harness({ confirm: () => accepted });
  const current = provider({
    ownerRevision: "owner-a",
    options: [{ key: "paid", label: "Paid", confirms: { de: "Costs credits" }, on: false }],
  });
  const box = await view.draw(current);
  await control(box).select("on");
  assert.equal(view.prompts.length, 1);
  assert.equal(view.prompts[0][1], "Costs credits");
  assert.equal(Object.keys(view.context.draft.extensionOptions).length, 0);
  accepted = true;
  await control(box).select("on");
  await view.save(current);
  assert.deepEqual(view.calls[0].input.confirmed, ["paid"]);
  assert.equal(view.calls[0].input.values.paid, true);
  assert.equal(view.calls[0].input.ownerRevision, "owner-a");
});

test("a declared blank translation map still obtains an explicit acknowledgement", async () => {
  const view = harness();
  const current = provider({
    options: [{ key: "paid", label: "Paid", confirms: { de: "" }, on: false }],
  });
  await control(await view.draw(current)).select("on");
  assert.equal(view.prompts.length, 1);
  assert.ok(view.prompts[0][1].includes("Paid"));
  await view.save(current);
  assert.deepEqual(view.calls[0].input.confirmed, ["paid"]);
});

function savingHarness() {
  let version = "0", on = false, failed = false;
  const view = harness({ api: async (route, input) => {
    if (route === "provider/extension-options") {
      assert.equal(input.expected, version, "retry must use the acknowledged configuration version");
      on = input.values.paid;
      version = String(Number(version) + 1);
      return { version };
    }
    if (route === "provider/save" && !failed) {
      failed = true;
      throw new Error("ordinary provider save failed");
    }
    return {};
  } });
  return { ...view, stored: () => ({ version, on }) };
}

test("successful option CAS survives a later provider save failure without resubmitting it", async () => {
  const view = savingHarness(), current = provider();
  await control(await view.draw(current)).select("on");
  await view.save(current);
  await view.context.providerAction("save", { id: current.id }, "Saved");
  assert.equal(view.errors.length, 1);
  await view.save(current);
  assert.deepEqual(view.stored(), { version: "1", on: true });
  assert.equal(view.calls.filter(call => call.route === "provider/extension-options").length, 1);
  assert.equal(Object.keys(view.context.draft.extensionOptions).length, 0);
});

test("turning a paid option off after a later provider failure persists false at the new CAS version", async () => {
  const view = savingHarness(), current = provider(), box = await view.draw(current);
  await control(box).select("on");
  await view.save(current);
  await view.context.providerAction("save", { id: current.id }, "Saved");
  await control(box).select("off");
  await view.save(current);
  assert.deepEqual(view.stored(), { version: "2", on: false });
  assert.equal(view.calls.filter(call => call.route === "provider/extension-options")[1].input.expected, "1");
});

test("new edits during CAS stop the ordinary save from closing their editor", async () => {
  let finish, arrived;
  const submitted = new Promise(resolve => { arrived = resolve; });
  const view = harness({ api: async (_route, input) => {
    if (input.expected === "0") {
      arrived();
      await new Promise(resolve => { finish = resolve; });
      return { version: "1" };
    }
    return { version: "2" };
  } });
  const current = provider(), box = await view.draw(current);
  await control(box).select("on");
  const editingDraft = view.context.draft;
  const saving = view.save(current).then(() => view.context.providerAction("save", { id: current.id }, "Saved"));
  await submitted;
  await control(box).select("off");
  finish();
  await assert.rejects(saving, /newer edits/i);
  assert.equal(view.context.draft, editingDraft);
  assert.equal(view.calls.some(call => call.route === "provider/save"), false);
  assert.equal(view.context.draft.extensionOptions.paid, false);
  await view.save(current);
  assert.deepEqual(view.calls.map(call => call.input.values), [{ paid: true }, { paid: false }]);
  assert.equal(view.calls[1].input.expected, "1");
});

test("a CAS completion cannot consume edits in another provider editor", async () => {
  let finish, arrived;
  const submitted = new Promise(resolve => { arrived = resolve; });
  const view = harness({ api: async () => {
    arrived();
    await new Promise(resolve => { finish = resolve; });
    return { version: "1" };
  } });
  const current = provider();
  await control(await view.draw(current)).select("on");
  const previous = view.context.draft;
  const saving = view.save(current).then(() => view.context.providerAction("save", { id: current.id }, "Saved"));
  await submitted;
  const next = { extensionOptions: { other: true } };
  view.context.draft = next;
  finish();
  await assert.rejects(saving, /changed/i);
  assert.equal(view.context.draft, next);
  assert.deepEqual(next.extensionOptions, { other: true });
  assert.equal(Object.keys(previous.extensionOptions).length, 0);
  assert.equal(view.calls.some(call => call.route === "provider/save"), false);
});

test("a lost CAS receipt makes the option state unknown even if a later click removes the delta", async () => {
  let on = false;
  const view = harness({ api: async (_route, input) => {
    on = input.values.paid;
    throw new Error("response lost after commit");
  } });
  const current = provider(), box = await view.draw(current);
  await control(box).select("on");
  await assert.rejects(view.save(current), /response lost/);
  assert.equal(on, true);
  await control(box).select("off");
  await assert.rejects(view.save(current), /changed|reopen|unknown/i);
  assert.equal(view.calls.length, 1, "unknown paid state must not be treated as an unchanged false value");
});

test("cached reopen cannot discard unknown paid state and a fresh unchanged save fences a late old CAS", async () => {
  let version = "0", on = false, first = true;
  const view = harness({ api: async (_route, input) => {
    if (first) {
      first = false;
      throw new Error("response lost before the old request finished");
    }
    assert.equal(input.expected, version);
    on = input.values.paid;
    version = String(Number(version) + 1);
    return { version };
  } });
  const cached = provider();
  await control(await view.draw(cached)).select("on");
  await assert.rejects(view.save(cached), /response lost/);
  view.context.draft = {};
  await view.draw(cached);
  await assert.rejects(view.save(cached), /changed|reopen|unknown/i);
  assert.equal(view.calls.length, 1);
  const fresh = provider();
  assert.equal(await view.reopen(cached, fresh), 1, "ordinary reopen must read fresh SDK metadata after a lost receipt");
  view.context.draft = {};
  await view.draw(fresh);
  await view.save(fresh);
  if (version === "0") {
    on = true;
    version = "1";
  }
  assert.equal(on, false, "a delayed old enable must lose CAS against the fresh saved choice");
  assert.equal(version, "1");
});

test("ordinary reopen refreshes provider metadata after an unknown options result", async () => {
  const view = harness(), cached = provider(), fresh = provider({ configVersion: "1" });
  cached.extensionOptionsUnknown = true;
  assert.equal(await view.reopen(cached, fresh), 1);
});

test("new option edits while ordinary provider save is pending keep their editor and next CAS", async () => {
  let finish, arrived, version = "0", on = false;
  const submitted = new Promise(resolve => { arrived = resolve; });
  const view = harness({ api: async (route, input) => {
    if (route === "provider/extension-options") {
      assert.equal(input.expected, version);
      on = input.values.paid;
      version = String(Number(version) + 1);
      return { version };
    }
    if (route === "provider/save") {
      arrived();
      await new Promise(resolve => { finish = resolve; });
    }
    return {};
  } });
  const current = provider(), box = await view.draw(current), editingDraft = view.context.draft;
  await control(box).select("on");
  await view.save(current);
  const saving = view.context.providerAction("save", { id: current.id }, "Saved");
  await submitted;
  await control(box).select("off");
  finish();
  await saving;
  assert.equal(view.context.draft, editingDraft, "ordinary provider completion discarded a newer option edit");
  assert.equal(editingDraft.extensionOptions.paid, false);
  await view.save(current);
  assert.equal(on, false);
  assert.equal(version, "2");
});

test("ordinary provider completion cannot close a different editor opened while it was pending", async () => {
  let finish, arrived;
  const submitted = new Promise(resolve => { arrived = resolve; });
  const view = harness({ api: async route => {
    if (route === "provider/save") {
      arrived();
      await new Promise(resolve => { finish = resolve; });
    }
    return route === "provider/extension-options" ? { version: "1" } : {};
  } });
  const current = provider();
  await control(await view.draw(current)).select("on");
  await view.save(current);
  const saving = view.context.providerAction("save", { id: current.id }, "Saved");
  await submitted;
  const next = { name: "another unsaved editor" };
  view.context.draft = next;
  finish();
  await saving;
  assert.equal(view.context.draft, next);
  assert.equal(next.name, "another unsaved editor");
});
