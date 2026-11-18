const fs = require("node:fs/promises");
const path = require("node:path");

async function closeWithScreenshots(browser, pages, directory, fileName) {
  try {
    if (directory) {
      await fs.mkdir(directory, { recursive: true });
      for (const [index, page] of pages.entries()) {
        if (!page.isClosed()) await page.screenshot({ path: path.join(directory, fileName(index)) });
      }
    }
  } finally {
    await browser.close();
  }
}

module.exports = { closeWithScreenshots };
