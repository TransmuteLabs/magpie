const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");
const { createHash } = require("node:crypto");
const { getEventListeners } = require("node:events");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { test } = require("node:test");
const vm = require("node:vm");
const { loadOfflineBaseline } = require("./fixtures/mobile-baseline.cjs");

const commit = "31dcfcc3369544f0a9f7acdb3ec02cd7cbf3c771";
const assetsGitTree = "bb5e257a75ee6fd749d6d0bd186759a073d80cb5";
const digest = bytes => createHash("sha256").update(bytes).digest("hex");

async function fixture(t, count = 2) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "mobile-baseline-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const manifest = { schema: 1, provenance: { commit, assetsGitTree }, files: [] };
  for (let index = 0; index < count; index++) {
    const relative = index ? `icons/icon-${index}.svg` : "index.html";
    const bytes = Buffer.from(index ? `<svg>unit-${index}</svg>` : "<html>unit baseline</html>");
    const file = path.join(root, "files", relative);
    await fs.mkdir(path.dirname(file), { recursive: true });
    await fs.writeFile(file, bytes, { mode: 0o644 });
    manifest.files.push({ path: relative, type: "file", mode: "100644", sha256: digest(bytes) });
  }
  const manifestFile = path.join(root, "manifest.json");
  const write = async () => {
    await fs.writeFile(manifestFile, JSON.stringify(manifest));
    return digest(await fs.readFile(manifestFile));
  };
  return { root, manifest, manifestFile, write, pin: await write() };
}

async function mobile(env = {}, options = {}) {
  const registered = [];
  const gitCalls = [];
  const launches = [];
  const stop = new Error("unit browser boundary");
  const body = Buffer.from("<html>pinned Git baseline</html>");
  const invokeGit = options.git || ((command, args) => {
    if (args[0] === "rev-parse") return Buffer.from(commit + "\n");
    if (args[0] === "ls-tree") return Buffer.from("internal/gui/assets/index.html\n");
    if (args[0] === "cat-file") return Buffer.concat([Buffer.from(`${"a".repeat(40)} blob ${body.length}\n`), body, Buffer.from("\n")]);
    throw new Error("unexpected Git call");
  });
  const launch = async () => { launches.push(true); throw stop; };
  const context = vm.createContext({
    __dirname: options.directory || __dirname,
    AbortController, URL,
    process: { env: { BROWSER: "chromium", ...env } },
    require(name) {
      if (name === "node:test") return { test: (name, run) => registered.push({ name, run }) };
      if (name === "node:child_process") return { execFileSync: (...args) => {
        gitCalls.push(args);
        return invokeGit(...args);
      } };
      if (name === "playwright") return { chromium: { launch }, webkit: { launch } };
      if (name === "playwright-core/lib/utilsBundle") return { PNG: {} };
      if (name === "./reader.cjs") return {};
      if (name === "./fixtures/mobile-baseline.cjs") return { loadOfflineBaseline };
      return require(name);
    },
  });
  const file = process.env.MOBILE_ADMISSION_SOURCE || path.join(__dirname, "mobile-web.test.cjs");
  vm.runInContext(await fs.readFile(file, "utf8"), context, { filename: file });
  assert.equal(registered.length, 1);
  return {
    context, gitCalls, launches, stop,
    run: () => registered[0].run({ after() {} }),
    prepare: () => vm.runInContext("prepareBaseline()", context),
    read: name => vm.runInContext(`baselineFile(${JSON.stringify(name)})`, context),
    published: () => vm.runInContext("baseline !== undefined", context),
    serve(route) { context.route = route; return vm.runInContext('server("en")(route)', context); },
    close(...pages) { context.pages = pages; return vm.runInContext("closeMobilePages(...pages)", context); },
    owner(page) { context.page = page; return vm.runInContext("mobileRouteOwner(page)", context); },
  };
}

for (const [name, selected] of [
  ["empty path absent pin", { MOBILE_BASELINE_MANIFEST: "" }],
  ["absent path empty pin", { MOBILE_BASELINE_MANIFEST_SHA256: "" }],
  ["both empty", { MOBILE_BASELINE_MANIFEST: "", MOBILE_BASELINE_MANIFEST_SHA256: "" }],
  ["valid path absent pin", { MOBILE_BASELINE_MANIFEST: "/unit/manifest.json" }],
  ["absent path valid pin", { MOBILE_BASELINE_MANIFEST_SHA256: "a".repeat(64) }],
  ["valid path empty pin", { MOBILE_BASELINE_MANIFEST: "/unit/manifest.json", MOBILE_BASELINE_MANIFEST_SHA256: "" }],
  ["empty path valid pin", { MOBILE_BASELINE_MANIFEST: "", MOBILE_BASELINE_MANIFEST_SHA256: "a".repeat(64) }],
]) {
  test(`explicit offline input refuses ${name} before Git or browser`, async () => {
    const current = await mobile({ BASE_REF: commit, ...selected });
    const error = await current.run().then(() => null, error => error);
    assert.equal(current.gitCalls.length, 0, "explicit offline input fell back to Git");
    assert.equal(current.launches.length, 0, "invalid offline input reached browser launch");
    assert.match(error?.message || "", /offline baseline requires both/);
    assert.equal(current.published(), false);
  });
}

for (const count of [1, 3]) {
  test(`valid ${count}-file offline input admits checked snapshots without Git`, async t => {
    const input = await fixture(t, count);
    const current = await mobile({ MOBILE_BASELINE_MANIFEST: input.manifestFile, MOBILE_BASELINE_MANIFEST_SHA256: input.pin, BASE_REF: commit });
    await assert.rejects(current.run(), error => error === current.stop);
    assert.equal(current.gitCalls.length, 0);
    assert.equal(current.launches.length, 1);
    assert.equal(vm.runInContext("baseline.size", current.context), count);
    const admitted = Buffer.from(current.read("index.html"));
    await fs.writeFile(path.join(input.root, "files/index.html"), "later on-disk replacement");
    await current.prepare();
    assert.deepEqual(current.read("index.html"), admitted);
  });
}

const invalid = [
  ["malformed independent pin", async input => "wrong", /independently pinned/],
  ["modified manifest under original pin", async input => {
    input.manifest.provenance.commit = "b".repeat(40);
    await input.write();
    return input.pin;
  }, /manifest SHA256 differs/],
  ["malformed JSON", async input => {
    const bytes = Buffer.from("{");
    await fs.writeFile(input.manifestFile, bytes);
    return digest(bytes);
  }, /JSON/],
  ["unsupported schema", async input => { input.manifest.schema = 2; return input.write(); }, /schema/],
  ["missing provenance", async input => { delete input.manifest.provenance; return input.write(); }, /provenance/],
  ["invalid provenance", async input => { input.manifest.provenance.commit = "wrong"; return input.write(); }, /provenance commit/],
  ["invalid tree", async input => { input.manifest.provenance.assetsGitTree = "wrong"; return input.write(); }, /provenance tree/],
  ["empty inventory", async input => { input.manifest.files = []; return input.write(); }, /nonempty array/],
  ["duplicate path", async input => { input.manifest.files.push(input.manifest.files[0]); return input.write(); }, /duplicate/],
  ["nonfile entry", async input => { input.manifest.files[0].type = "tree"; return input.write(); }, /regular file/],
  ["invalid mode", async input => { input.manifest.files[0].mode = "120000"; return input.write(); }, /mode/],
  ["wrong filesystem mode", async input => { await fs.chmod(path.join(input.root, "files/index.html"), 0o755); return input.pin; }, /mode differs/],
  ["invalid content pin", async input => { input.manifest.files[0].sha256 = "wrong"; return input.write(); }, /invalid baseline SHA256/],
  ["missing declared file", async input => { await fs.unlink(path.join(input.root, "files/icons/icon-1.svg")); return input.pin; }, /ENOENT/],
  ["unreadable declared file", async input => { await fs.chmod(path.join(input.root, "files/index.html"), 0); return input.pin; }, /mode differs|EACCES/],
  ["modified declared file", async input => { await fs.writeFile(path.join(input.root, "files/icons/icon-1.svg"), "wrong"); return input.pin; }, /SHA256 differs/],
  ["file directory collision", async input => {
    input.manifest.files.push({ ...input.manifest.files[0], path: "index.html/child.svg" });
    return input.write();
  }, /collision/],
  ["file symlink", async input => {
    await fs.unlink(path.join(input.root, "files/index.html"));
    await fs.symlink("icons/icon-1.svg", path.join(input.root, "files/index.html"));
    return input.pin;
  }, /regular file/],
  ["directory symlink", async input => {
    await fs.rename(path.join(input.root, "files/icons"), path.join(input.root, "elsewhere"));
    await fs.symlink("../elsewhere", path.join(input.root, "files/icons"));
    return input.pin;
  }, /directory/],
  ["files root symlink", async input => {
    await fs.rename(path.join(input.root, "files"), path.join(input.root, "elsewhere"));
    await fs.symlink("elsewhere", path.join(input.root, "files"));
    return input.pin;
  }, /directory/],
];
for (const relative of ["../index.html", "/index.html", "icons//a.svg", "icons/./a.svg", "C:/index.html", "icons\\a.svg", "bad\0name"]) {
  invalid.push([`invalid path ${JSON.stringify(relative)}`, async input => {
    input.manifest.files[0].path = relative;
    return input.write();
  }, /path/]);
}
for (const [name, mutate, message] of invalid) {
  test(`offline admission refuses ${name} without fallback or partial publication`, async t => {
    const input = await fixture(t);
    const pin = await mutate(input);
    const current = await mobile({ MOBILE_BASELINE_MANIFEST: input.manifestFile, MOBILE_BASELINE_MANIFEST_SHA256: pin });
    await assert.rejects(current.run(), message);
    assert.equal(current.gitCalls.length, 0);
    assert.equal(current.launches.length, 0);
    assert.equal(current.published(), false);
  });
}

test("offline admission refuses conflicting BASE_REF before launch", async t => {
  const input = await fixture(t);
  const current = await mobile({ MOBILE_BASELINE_MANIFEST: input.manifestFile, MOBILE_BASELINE_MANIFEST_SHA256: input.pin, BASE_REF: "other-ref" });
  await assert.rejects(current.run(), /BASE_REF conflicts/);
  assert.equal(current.gitCalls.length, 0);
  assert.equal(current.launches.length, 0);
  assert.equal(current.published(), false);
});

test("missing manifest refuses before Git or browser", async t => {
  const input = await fixture(t);
  const current = await mobile({ MOBILE_BASELINE_MANIFEST: path.join(input.root, "absent.json"), MOBILE_BASELINE_MANIFEST_SHA256: input.pin });
  await assert.rejects(current.run(), /ENOENT/);
  assert.equal(current.gitCalls.length, 0);
  assert.equal(current.launches.length, 0);
});

test("both absent inputs resolve checkout ref once and pin every subsequent read", async () => {
  const current = await mobile({ BASE_REF: "moving-reference" });
  await assert.rejects(current.run(), error => error === current.stop);
  assert.equal(current.launches.length, 1);
  assert.deepEqual(current.gitCalls.map(call => call[1][0]), ["rev-parse", "ls-tree", "cat-file"]);
  assert.equal(current.gitCalls[0][1].at(-1), "moving-reference^{commit}");
  assert.equal(current.gitCalls[1][1][3], commit);
  assert.equal(current.gitCalls[2][2].input, `${commit}:internal/gui/assets/index.html\n`);
  assert.equal(current.read("index.html").toString(), "<html>pinned Git baseline</html>");
});

for (const [name, invoke, message] of [
  ["failed resolve", () => { throw new Error("unknown checkout ref"); }, /unknown checkout ref/],
  ["empty inventory", (command, args) => Buffer.from(args[0] === "rev-parse" ? commit + "\n" : ""), /assets are empty/],
  ["missing object", (command, args) => Buffer.from(args[0] === "rev-parse" ? commit + "\n" : args[0] === "ls-tree" ? "internal/gui/assets/index.html\n" : "missing missing\n"), /missing or not a blob/],
  ["truncated object", (command, args) => Buffer.from(args[0] === "rev-parse" ? commit + "\n" : args[0] === "ls-tree" ? "internal/gui/assets/index.html\n" : `${"a".repeat(40)} blob 900\nshort`), /truncated baseline Git blob/],
]) {
  test(`checkout fallback refuses ${name} before browser or partial publication`, async () => {
    const current = await mobile({ BASE_REF: commit }, { git: invoke });
    await assert.rejects(current.run(), message);
    assert.equal(current.launches.length, 0);
    assert.equal(current.published(), false);
  });
}

if (process.env.MOBILE_ADMISSION_CHECKOUT) {
  test("actual checkout fallback reads pinned31dc without offline variables", async () => {
    const current = await mobile({ BASE_REF: commit }, { directory: path.join(process.env.MOBILE_ADMISSION_CHECKOUT, "internal/gui/tests"), git: execFileSync });
    await current.prepare();
    assert.equal(vm.runInContext("baseline.size", current.context), 337);
    assert(current.read("app.js").includes("function connectPanel"));
    assert.deepEqual(current.gitCalls.map(call => call[1][0]), ["rev-parse", "ls-tree", "cat-file"]);
    assert.equal(current.launches.length, 0);
  });
  test("actual checkout fallback refuses missing ref without launch", async () => {
    const current = await mobile({ BASE_REF: "0".repeat(40) }, { directory: path.join(process.env.MOBILE_ADMISSION_CHECKOUT, "internal/gui/tests"), git: execFileSync });
    await assert.rejects(current.run(), /Command failed|reference|revision/);
    assert.equal(current.launches.length, 0);
  });
}

test("actual Git-less fallback refuses instead of serving missing baseline assets", async t => {
  const input = await fixture(t);
  const current = await mobile({ BASE_REF: commit }, { directory: path.join(input.root, "internal/gui/tests"), git: execFileSync });
  await assert.rejects(current.run(), /not a git repository|Command failed/);
  assert.equal(current.launches.length, 0);
});

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function quotaPage(closeMessage = reason => reason) {
  const readiness = deferred();
  const closing = deferred();
  const replies = [];
  const closedError = Object.assign(new Error("controlled target close"), { name: "TargetClosedError" });
  let closed = false;
  const page = {
    context: () => ({ close: options => page.close(options) }),
    locator(selector) {
      assert.equal(selector, "#usageAgents .row.stat");
      return { first: () => ({ waitFor(options) {
        assert.equal(options.state, "attached");
        assert.equal(options.timeout, 30000);
        return readiness.promise;
      } }) };
    },
    isClosed: () => closed,
    async close({ reason }) {
      await closing.promise;
      closed = true;
      closedError.message = closeMessage(reason);
      readiness.reject(closedError);
    },
  };
  const route = {
    request: () => ({ url: () => "http://magpie.test/api/usage/quotas", frame: () => ({ page: () => page }) }),
    fulfill: reply => { replies.push(reply); return Promise.resolve(); },
  };
  return { page, route, readiness, closing, replies };
}

test("quota closing cancels readiness and joins its underlying wait and callback", async () => {
  const current = await mobile();
  const input = quotaPage();
  const serving = current.serve(input.route);
  const owner = current.owner(input.page);
  assert.equal(owner.waits.size, 1);
  assert.equal(owner.routes.size, 1);
  let joined = false;
  const closing = current.close(input.page).then(() => { joined = true; });
  await serving;
  assert.equal(owner.closing, true);
  assert.equal(owner.cancel.signal.aborted, true);
  assert.equal(joined, false, "cleanup returned before the underlying page/wait closed");
  assert.deepEqual(input.replies, [], "closing quota route must not publish a response");
  input.closing.resolve();
  await closing;
  assert.equal(owner.waits.size, 0);
  assert.equal(owner.routes.size, 0);
  assert.equal(getEventListeners(owner.cancel.signal, "abort").length, 0);
});

test("quota readiness on a live page preserves its ordinary empty response", async () => {
  const current = await mobile();
  const input = quotaPage();
  const serving = current.serve(input.route);
  input.readiness.resolve();
  await serving;
  assert.equal(input.replies.length, 1);
  assert.equal(input.replies[0].json.length, 0);
  input.closing.resolve();
  await current.close(input.page);
});

test("quota closing recognizes the exact close reason before Playwright call logs", async () => {
  const current = await mobile();
  const input = quotaPage(reason => `locator.waitFor: ${reason}\nCall log:\n  - waiting for locator('#usageAgents .row.stat').first()\n`);
  const serving = current.serve(input.route);
  input.closing.resolve();
  await current.close(input.page);
  await serving;
  assert.equal(current.owner(input.page).waits.size, 0);
  assert.equal(current.owner(input.page).routes.size, 0);
  assert.deepEqual(input.replies, []);
});

for (const [name, message] of [
  ["reason mentioned only in a call log", reason => `unrelated operation failure\nCall log:\n  - ${reason}`],
  ["different close reason", reason => `locator.waitFor: ${reason} extra\nCall log:\n`],
]) {
  test(`closing preserves ${name}`, async () => {
    const current = await mobile();
    const input = quotaPage();
    const serving = current.serve(input.route);
    const closing = current.close(input.page);
    await serving;
    const failure = new Error(message(current.owner(input.page).reason));
    input.readiness.reject(failure);
    input.closing.resolve();
    await assert.rejects(closing, error => error === failure);
  });
}

for (const name of ["Error", "TimeoutError", "TargetClosedError"]) {
  test(`live quota ${name} retains exact failure identity through cleanup`, async () => {
    const current = await mobile();
    const input = quotaPage();
    const failure = Object.assign(new Error("live quota failure"), { name });
    const serving = current.serve(input.route);
    input.readiness.reject(failure);
    await assert.rejects(serving, error => error === failure);
    assert.equal(current.owner(input.page).closing, false);
    input.closing.resolve();
    await assert.rejects(current.close(input.page), error => error === failure);
    assert.deepEqual(input.replies, []);
  });
}

test("closing does not hide a non-close waiter error after page closure", async () => {
  const current = await mobile();
  const input = quotaPage();
  const failure = new Error("unrelated failure during close");
  const serving = current.serve(input.route);
  const closing = current.close(input.page);
  await serving;
  input.readiness.reject(failure);
  input.closing.resolve();
  await assert.rejects(closing, error => error === failure);
});

test("closing does not accept an unmarked target-closed error", async () => {
  const current = await mobile();
  const input = quotaPage();
  const failure = Object.assign(new Error("Target page, context or browser has been closed"), { name: "TargetClosedError" });
  const serving = current.serve(input.route);
  const closing = current.close(input.page);
  await serving;
  input.readiness.reject(failure);
  input.closing.resolve();
  await assert.rejects(closing, error => error === failure);
});

test("closing one page leaves a sibling quota request live", async () => {
  const current = await mobile();
  const first = quotaPage(), second = quotaPage();
  const one = current.serve(first.route), two = current.serve(second.route);
  first.closing.resolve();
  await current.close(first.page);
  await one;
  assert.equal(current.owner(second.page).closing, false);
  second.readiness.resolve();
  await two;
  assert.equal(second.replies.length, 1);
  second.closing.resolve();
  await current.close(second.page);
});

test("multi-page cleanup joins a sibling even when another route has failed", async () => {
  const current = await mobile();
  const first = quotaPage(), second = quotaPage();
  const failure = new Error("live first-page failure");
  const one = current.serve(first.route), two = current.serve(second.route);
  first.readiness.reject(failure);
  await assert.rejects(one, error => error === failure);
  let returned = false;
  const closing = current.close(first.page, second.page).catch(error => { returned = true; throw error; });
  await two;
  first.closing.resolve();
  await Promise.resolve();
  assert.equal(returned, false, "cleanup abandoned its sibling page");
  second.closing.resolve();
  await assert.rejects(closing, error => error === failure);
  assert.equal(current.owner(second.page).waits.size, 0);
  assert.equal(current.owner(second.page).routes.size, 0);
});
