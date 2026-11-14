import assert from "node:assert/strict";
import test from "node:test";
import { cp, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { join, posix } from "node:path";
import { tmpdir } from "node:os";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = fileURLToPath(new URL("../", import.meta.url));

test("SDK distribution includes every reachable runtime and type dependency", async () => {
  const metadata = JSON.parse(await readFile(join(root, "package.json"), "utf8"));
  const included = new Set(metadata.files);
  const pending = Object.values(metadata.exports).flatMap(entry => Object.values(entry));
  const seen = new Set();
  while (pending.length) {
    const relative = posix.normalize(pending.pop().replace(/^\.\//, ""));
    if (seen.has(relative)) continue;
    seen.add(relative);
    assert(included.has(relative), `SDK distribution omits imported authoring file ${relative}`);
    const source = await readFile(join(root, relative), "utf8");
    for (const match of source.matchAll(/(?:from\s*|import\s*)["'](\.[^"']+)["']/g)) {
      let dependency = posix.normalize(posix.join(posix.dirname(relative), match[1]));
      if (relative.endsWith(".d.ts") && dependency.endsWith(".js")) {
        const declaration = dependency.replace(/\.js$/, ".d.ts");
        if (await stat(join(root, declaration)).then(() => true, () => false)) dependency = declaration;
      }
      pending.push(dependency);
    }
  }
  const distribution = await mkdtemp(join(tmpdir(), "magpie-sdk-package-"));
  try {
    await writeFile(join(distribution, "package.json"), JSON.stringify(metadata));
    for (const relative of included) {
      await cp(join(root, relative), join(distribution, relative), { recursive: true });
    }
    const imported = await import(pathToFileURL(join(distribution, "index.js")).href);
    assert.equal(imported.CONTRACT.version.minor, 9);
    assert.equal(typeof imported.validateLedgerObservation, "function");
    assert.equal(typeof imported.validateCatalogProjection, "function");
    assert.equal(typeof imported.validateStorageRequest, "function");
    const ui = await import(pathToFileURL(join(distribution, "ui.js")).href);
    assert.equal(typeof ui, "object");
  } finally {
    await rm(distribution, { recursive: true, force: true });
  }
});
