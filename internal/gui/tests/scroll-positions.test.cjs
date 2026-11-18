const assert = require("node:assert/strict");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { scrollPositions } = require(process.env.MAGPIE_SCROLL_READER || "./reader.cjs");

const vertical = { axes: "y", windowAxes: "y", positiveY: true };
const routing = { axes: "y", windowAxes: "", positiveY: false };
const models = { axes: "y", windowAxes: "xy", positiveY: true };
const folded = { ...vertical, exclude: ".mchips" };
const frame = (body) => `<!doctype html><html><head><style>
body { margin:0 }
.box { width:120px;height:80px;overflow:auto }
.content { width:600px;height:500px }
</style></head><body>${body}</body></html>`;
const box = (id, extra = "") => `<div id="${id}" class="box ${extra}"><div class="content"></div></div>`;
const read = async (page, options) => JSON.parse(await scrollPositions(page, options));
const move = async (page, selector, left, top) => {
  const actual = await page.locator(selector).evaluate((node, value) => {
    node.scrollLeft = value.left;
    node.scrollTop = value.top;
    return [node.scrollLeft, node.scrollTop];
  }, { left, top });
  assert.deepEqual(actual, [left, top], "the actual DOM establishes the exact scroll vector");
};

const cases = {
  "approved default class pixels and owner": async (page) => {
    await page.setContent(frame('<i></i><div class="box holds"><div class="content"></div></div><b></b><div class="box other"><div class="content"></div></div>'));
    await move(page, ".holds", 7, 100);
    const before = await scrollPositions(page);
    assert.deepEqual(JSON.parse(before), [["window", 0, 0], ["1.1", 7, 100]]);
    await page.locator(".holds").evaluate((node) => { node.className = "box changed"; });
    assert.equal(await scrollPositions(page), before, "class-only bookkeeping keeps the default snapshot");
    await move(page, ".changed", 7, 101);
    assert.notEqual(await scrollPositions(page), before, "default detects exactly one vertical pixel");
    await move(page, ".changed", 8, 100);
    assert.notEqual(await scrollPositions(page), before, "default detects exactly one horizontal pixel");
    await move(page, ".changed", 0, 0);
    await move(page, ".other", 7, 100);
    assert.notEqual(await scrollPositions(page), before, "the same offsets on another structural owner differ");
    assert.deepEqual(await read(page), [["window", 0, 0], ["1.3", 7, 100]]);
  },
  "membership before Y projection": async (page) => {
    await page.setContent(frame('<div id="horizontal" class="box"><div style="width:600px;height:20px"></div></div>' + box("vertical")));
    await move(page, "#horizontal", 13, 0);
    await move(page, "#vertical", 0, 17);
    assert.deepEqual(await read(page), [["window", 0, 0], ["#horizontal", 13, 0], ["#vertical", 0, 17]]);
    assert.deepEqual(await read(page, vertical), [["window", 0], ["#vertical", 17]], "horizontal-only owners do not enter a positive-Y snapshot");
    const before = await scrollPositions(page, vertical);
    await move(page, "#horizontal", 14, 0);
    assert.equal(await scrollPositions(page, vertical), before, "excluded element X remains excluded");
    await move(page, "#vertical", 0, 18);
    assert.notEqual(await scrollPositions(page, vertical), before, "included Y detects exactly one pixel");
    assert.deepEqual(await read(page, models), [["window", 0, 0], ["#vertical", 18]], "windowXY is independent of elementY");
  },
  "independent window and document membership": async (page) => {
    await page.setContent('<!doctype html><html id="document-owner"><head><style>body{margin:0}#space{width:2400px;height:2400px}</style></head><body><div id="space"></div></body></html>');
    await page.evaluate(() => window.scrollTo(19, 23));
    await page.waitForFunction(() => window.scrollX === 19 && window.scrollY === 23);
    assert.deepEqual(await page.evaluate(() => [document.scrollingElement.id, document.scrollingElement.scrollLeft, document.scrollingElement.scrollTop]), ["document-owner", 19, 23]);
    assert.deepEqual(await read(page, vertical), [["window", 23], ["#document-owner", 23]]);
    assert.deepEqual(await read(page, models), [["window", 19, 23], ["#document-owner", 23]]);
    assert.deepEqual(await read(page, routing), [["#document-owner", 23]], "omitting window does not omit the document element");
    const yBefore = await scrollPositions(page, vertical);
    const xyWindowBefore = await scrollPositions(page, models);
    await page.evaluate(() => window.scrollTo(20, 23));
    await page.waitForFunction(() => window.scrollX === 20 && window.scrollY === 23);
    assert.equal(await scrollPositions(page, vertical), yBefore, "windowX remains outside windowY");
    assert.notEqual(await scrollPositions(page, models), xyWindowBefore, "windowXY still detects exactly one X pixel");
    const documentBefore = await scrollPositions(page, routing);
    await page.evaluate(() => window.scrollTo(20, 24));
    await page.waitForFunction(() => window.scrollX === 20 && window.scrollY === 24);
    assert.notEqual(await scrollPositions(page, routing), documentBefore, "the included document owner still detects Y");
  },
  "positive versus nonzero signed Y": async (page) => {
    await page.setContent(frame('<div id="negative" class="box" style="display:flex;flex-direction:column-reverse"><div style="flex:none;height:500px;width:60px"></div></div>' + box("positive")));
    await move(page, "#negative", 0, -9);
    await move(page, "#positive", 0, 11);
    assert.deepEqual(await read(page), [["window", 0, 0], ["#negative", 0, -9], ["#positive", 0, 11]]);
    assert.deepEqual(await read(page, vertical), [["window", 0], ["#positive", 11]], "positiveTop excludes an actual negative offset");
    assert.deepEqual(await read(page, routing), [["#negative", -9], ["#positive", 11]], "truthyTop retains an actual negative offset");
    const positiveBefore = await scrollPositions(page, vertical);
    const nonzeroBefore = await scrollPositions(page, routing);
    await move(page, "#negative", 0, -10);
    assert.equal(await scrollPositions(page, vertical), positiveBefore);
    assert.notEqual(await scrollPositions(page, routing), nonzeroBefore);
  },
  "mchips excludes only its own node": async (page) => {
    await page.setContent(frame('<div id="chips" class="box mchips" style="width:220px;height:120px"><div class="content">' + box("descendant") + '</div></div>' + box("outside")));
    await move(page, "#chips", 0, 30);
    await move(page, "#descendant", 0, 12);
    await move(page, "#outside", 0, 21);
    assert.deepEqual(await read(page, folded), [["window", 0], ["#descendant", 12], ["#outside", 21]], "a scrolled descendant remains observed");
    const before = await scrollPositions(page, folded);
    await move(page, "#chips", 0, 31);
    assert.equal(await scrollPositions(page, folded), before, "only the mchips node is excluded");
    await move(page, "#descendant", 0, 13);
    assert.notEqual(await scrollPositions(page, folded), before, "an included descendant detects one pixel");
    await move(page, "#descendant", 0, 12);
    await move(page, "#outside", 0, 22);
    assert.notEqual(await scrollPositions(page, folded), before, "an outside owner still detects one pixel");
  },
  "full DOM identity despite filtering": async (page) => {
    await page.setContent(frame('<i id="repeated"></i><section id="excluded" class="box mchips" style="height:120px"><span></span><div class="box inner"><div class="content"></div></div><div style="height:500px"></div></section><b></b><div id="repeated" class="box outer"><div class="content"></div></div>'));
    await move(page, "#excluded", 0, 5);
    await move(page, ".inner", 0, 7);
    await move(page, ".outer", 0, 9);
    assert.deepEqual(await read(page, folded), [["window", 0], ["1.1.1", 7], ["1.3", 9]], "paths include non-scrolling and excluded siblings, and ID uniqueness is document-wide");
    const before = await scrollPositions(page, folded);
    await page.locator(".outer").evaluate((node) => { node.className = "box changed"; });
    assert.equal(await scrollPositions(page, folded), before);
  },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const [name, check] of Object.entries(cases)) {
    test(`${engine}: scroll positions ${name}`, async (t) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(() => browser.close());
      const page = await browser.newPage({ viewport: { width: 800, height: 600 } });
      page.setDefaultTimeout(5000);
      page.setDefaultNavigationTimeout(60000);
      const errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await check(page);
      assert.deepEqual(errors, [], "genuine page errors remain visible");
    });
  }
}
