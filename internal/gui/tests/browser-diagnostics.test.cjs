const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");
const { test } = require("node:test");
const helper = process.env.MAGPIE_DIAGNOSTIC_HELPER || require.resolve("./fixtures/browser-diagnostics.cjs");
const { closeWithScreenshots } = require(helper);

async function folder(t) {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), "gui-diagnostics-"));
  t.after(() => fs.rm(directory, { recursive: true, force: true }));
  return directory;
}

test("closed story tabs are skipped; open tabs retain their screenshot indexes", async (t) => {
  const directory = await folder(t), calls = [];
  const browser = { close: async () => calls.push("close") };
  const pages = [
    { isClosed: () => true, screenshot: async () => { throw Error("closed story page"); } },
    { isClosed: () => false, screenshot: async (options) => calls.push(options.path) },
  ];
  await closeWithScreenshots(browser, pages, directory, (index) => `story-${index}.png`);
  assert.deepEqual(calls, [path.join(directory, "story-1.png"), "close"]);
});

test("screenshot failure remains a failure and always closes the browser", async (t) => {
  const directory = await folder(t), calls = [], failure = Error("screenshot failed");
  const browser = { close: async () => calls.push("close") };
  const pages = [{ isClosed: () => false, screenshot: async () => { throw failure; } }];
  await assert.rejects(closeWithScreenshots(browser, pages, directory, () => "story.png"), (error) => error === failure);
  assert.deepEqual(calls, ["close"]);
});

test("artifact-directory failure remains a failure and always closes the browser", async (t) => {
  const directory = await folder(t), calls = [], file = path.join(directory, "file");
  await fs.writeFile(file, "");
  await assert.rejects(closeWithScreenshots({ close: async () => calls.push("close") }, [], path.join(file, "shots"), () => "story.png"));
  assert.deepEqual(calls, ["close"]);
});

test("without artifacts cleanup still closes the browser", async () => {
  const calls = [];
  await closeWithScreenshots({ close: async () => calls.push("close") }, [{
    isClosed: () => { throw Error("no diagnostic inspection requested"); },
  }], undefined, () => { throw Error("no diagnostic filename requested"); });
  assert.deepEqual(calls, ["close"]);
});

test("a failing body still fails through actual node:test teardown", async (t) => {
  const directory = await folder(t), marker = path.join(directory, "closed");
  const child = path.join(directory, "body-failure.test.cjs");
  await fs.writeFile(child, `
const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const { closeWithScreenshots } = require(${JSON.stringify(helper)});
test("original body assertion", async (t) => {
  t.after(() => closeWithScreenshots(
    { close: async () => fs.writeFile(${JSON.stringify(marker)}, "closed") },
    [{ isClosed: () => true, screenshot: async () => { throw Error("closed story page"); } }],
    ${JSON.stringify(path.join(directory, "shots"))}, () => "story.png"
  ));
  assert.equal("actual account mutation", "expected account mutation", "PRIMARY ACCOUNT ASSERTION");
});
`);
  const environment = { ...process.env };
  delete environment.NODE_TEST_CONTEXT;
  const result = spawnSync(process.execPath, ["--test", "--test-reporter=tap", child], { env: environment, encoding: "utf8", timeout: 10000 });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stdout, /PRIMARY ACCOUNT ASSERTION/);
  assert.equal(await fs.readFile(marker, "utf8"), "closed");
});
