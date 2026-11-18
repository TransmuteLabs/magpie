const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { closeWithScreenshots } = require("./fixtures/browser-diagnostics.cjs");
const { isPageLifecycleRequest } = require("./fixtures/page-lifecycle.cjs");

const assets = process.env.MAGPIE_HOLD_ASSETS || path.resolve(__dirname, "../assets");

async function open(engine, t, action) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
  const pages = [];
  t.after(() => closeWithScreenshots(browser, pages, process.env.ARTIFACT_DIR, () => `${engine}-${action}.png`));
  const page = await browser.newPage({ viewport: { width: 1100, height: 700 }, reducedMotion: "reduce" });
  pages.push(page);
  const errors = [], writes = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(60000);
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    const json = (body) => route.fulfill({ json: body });
    if (url.origin !== "http://magpie.test") return route.fulfill({ status: 404, body: "" });
    if (isPageLifecycleRequest(request)) return json({});
    if (!["GET", "HEAD", "OPTIONS"].includes(request.method())) writes.push(url.pathname);
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: 'window.bootPrefs={lang:"en",theme:"light",web:true};' });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({
      agents: [{ id: "fixture", name: "Fixture", fields: [] }], profiles: [], settings: { lang: "en", theme: "light", guideHidden: true },
    });
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], gateway: { running: true, window: true } });
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    return route.fulfill({
      body: await fs.readFile(file),
      contentType: { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)],
    });
  });
  await page.goto("http://magpie.test/?view=agents");
  await page.locator('.row.agent[data-id="fixture"]').waitFor();
  await page.evaluate(() => load());
  await page.locator("#view-agents").evaluate((view, action) => {
    held = null;
    view.style.display = "block";
    view.innerHTML = '<div id="above" style="height:400px"></div><section id="stable-parent" style="height:60px;display:flex;align-items:center"><div id="changing-wrapper" style="height:40px;display:flex;align-items:center"><button id="hold-probe" class="text">Apply</button></div><span style="display:block;height:20px">Label</span></section><div style="height:1500px"></div>';
    if (action === "flow") document.getElementById("stable-parent").style.display = "block";
    if (["above-redraw", "collapse-redraw", "above-grow-upward"].includes(action)) {
      const outer = document.createElement("section");
      view.prepend(outer);
      outer.append(document.getElementById("above"), document.getElementById("stable-parent"));
    }
    if (action === "stationary-auto-redraw") {
      document.getElementById("stable-parent").style.cssText = "display:flow-root";
      document.getElementById("changing-wrapper").style.cssText = "height:139px;margin-top:12px";
      document.getElementById("stable-parent").lastElementChild.style.cssText = "display:block;height:28px";
    }
    if (action === "fixed-growth-redraw") {
      const parent = document.getElementById("stable-parent");
      parent.style.cssText = "height:160px;display:block";
      const growth = document.createElement("div"), marker = document.createElement("div");
      growth.id = "internal-growth";
      growth.style.height = "80px";
      marker.id = "independent-marker";
      marker.style.height = "20px";
      parent.prepend(growth, marker);
    }
    const button = document.getElementById("hold-probe");
    button.onclick = () => {
      const box = document.getElementById("changing-wrapper");
      if (action === "above") document.getElementById("above").style.height = "480px";
      else if (action === "stationary-auto-redraw") {
        box.replaceChildren(document.createElement("span"));
        box.style.marginTop = "0";
        box.style.height = "180px";
      } else if (action === "fixed-growth-redraw") {
        document.getElementById("internal-growth").style.height = "120px";
        box.replaceChildren(document.createElement("span"));
        box.style.height = "60px";
      }
      else if (["above-redraw", "collapse-redraw", "above-grow-upward"].includes(action)) {
        box.replaceChildren(document.createElement("span"));
        box.style.height = action === "above-grow-upward" ? "160px" : "60px";
        document.getElementById("above").style.height = action === "collapse-redraw" ? "320px" : action === "above-grow-upward" ? "440px" : "480px";
      }
      else if (action === "replacement") {
        const children = [...view.children].map((child) => child.cloneNode(true));
        children[0].style.height = "450px";
        view.replaceChildren(...children);
      } else {
        box.replaceChildren(document.createElement("span"));
        box.style.height = action === "grow" ? "60px" : "20px";
      }
    };
  }, action);
  const bounds = await page.locator("#view-agents").boundingBox();
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + 100);
  await page.mouse.wheel(0, 200);
  await page.waitForFunction(() => document.querySelector("#view-agents").scrollTop >= 150);
  const position = () => page.locator(action === "fixed-growth-redraw" ? "#independent-marker" : "#stable-parent").evaluate((row) => ({
    top: row.getBoundingClientRect().top,
    scroll: row.closest(".view").scrollTop,
  }));
  const before = await position();
  const button = await page.locator("#hold-probe").boundingBox();
  assert.ok(button.y > bounds.y && button.y + button.height < bounds.y + bounds.height, "the click is wholly in view");
  await page.locator("#hold-probe").click();
  await page.evaluate(async () => {
    for (let frame = 0; frame < 4; frame++) await new Promise(requestAnimationFrame);
  });
  return { before, after: await position(), errors, writes };
}

for (const engine of ["chromium", "webkit"]) {
  for (const action of ["shrink", "grow", "flow", "above", "above-redraw", "collapse-redraw", "above-grow-upward", "replacement", "stationary-auto-redraw", "fixed-growth-redraw"]) {
    test(`${engine}: held ancestor after ${action}`, async (t) => {
      const { before, after, errors, writes } = await open(engine, t, action);
      if (["stationary-auto-redraw", "fixed-growth-redraw"].includes(action)) console.log("HOLD-BOUNDARY", JSON.stringify({ engine, action, before, after }));
      assert.equal(after.top, before.top, "the stable row moves under the pointer");
      assert.equal(after.scroll, before.scroll + ({ above: 80, "above-redraw": 80, "collapse-redraw": -80, "above-grow-upward": 40, replacement: 50, "fixed-growth-redraw": 40 }[action] || 0), "only layout above the held row is compensated");
      assert.deepEqual(errors, []);
      assert.deepEqual(writes, []);
    });
  }
}
