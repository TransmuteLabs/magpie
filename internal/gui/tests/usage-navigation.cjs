const { openPage } = require("./reader.cjs");

async function openUsageRequests(page) {
  const usage = page.locator('#nav button[data-view="usage"]');
  if (await usage.getAttribute("aria-current") !== "page") await openPage(page, "usage");
  await page.locator("#view-usage").waitFor({ state: "visible" });
  const source = page.locator('#usageTab > button[data-seg="requests"]');
  await source.waitFor({ state: "attached" });
  const folded = page.locator('#nav button[data-view="usage"][aria-current="page"][aria-expanded="false"]');
  if (await folded.count()) await folded.click();
  if (await source.isVisible()) await source.click();
  else {
    const name = (await source.textContent()).trim();
    await page.locator('.nav-sub[data-view="usage"]:not(.leaving)').getByRole("tab", { name, exact: true }).click();
  }
}

module.exports = { openUsageRequests };
