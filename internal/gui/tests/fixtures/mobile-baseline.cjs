const assert = require("node:assert/strict");
const { createHash } = require("node:crypto");
const fs = require("node:fs/promises");
const path = require("node:path");

async function loadOfflineBaseline(manifestFile, expectedSHA256) {
  assert.match(expectedSHA256, /^[a-f0-9]{64}$/, "an independently pinned baseline manifest SHA256 is required");
  const manifestBytes = await fs.readFile(manifestFile);
  assert.equal(createHash("sha256").update(manifestBytes).digest("hex"), expectedSHA256, "baseline manifest SHA256 differs from the scheduler binding");
  const manifest = JSON.parse(manifestBytes.toString("utf8"));
  assert.equal(manifest.schema, 1, "unsupported mobile baseline schema");
  assert(manifest.provenance && typeof manifest.provenance === "object", "baseline provenance is required");
  assert.match(manifest.provenance.commit, /^[a-f0-9]{40}$/, "invalid baseline provenance commit");
  assert.match(manifest.provenance.assetsGitTree, /^[a-f0-9]{40}$/, "invalid baseline provenance tree");
  assert(Array.isArray(manifest.files) && manifest.files.length, "baseline files must be a nonempty array");
  const names = new Set();
  const entries = manifest.files.map((entry) => {
    assert(entry && typeof entry === "object" && typeof entry.path === "string", "invalid baseline entry");
    const relative = entry.path;
    assert(relative && !/[\\\0:]/.test(relative), "invalid baseline path");
    assert(relative.split("/").every((part) => part && part !== "." && part !== ".."), "invalid baseline path component");
    assert(!names.has(relative), "duplicate baseline path");
    names.add(relative);
    assert.equal(entry.type, "file", "baseline entry is not a regular file");
    assert(["100644", "100755"].includes(entry.mode), "invalid baseline file mode");
    assert.match(entry.sha256, /^[a-f0-9]{64}$/, "invalid baseline SHA256");
    return { ...entry, relative };
  });
  for (const name of names) {
    const parts = name.split("/");
    for (let size = 1; size < parts.length; size++) assert(!names.has(parts.slice(0, size).join("/")), "baseline file/directory collision");
  }
  const root = path.join(path.dirname(path.resolve(manifestFile)), "files");
  const checked = new Set();
  const directory = async (dir) => {
    if (checked.has(dir)) return;
    assert((await fs.lstat(dir)).isDirectory(), "baseline directory is missing, a symlink or not a directory");
    checked.add(dir);
  };
  await directory(root);
  const loaded = new Map();
  for (const entry of entries) {
    const parts = entry.relative.split("/");
    let dir = root;
    for (const part of parts.slice(0, -1)) {
      dir = path.join(dir, part);
      await directory(dir);
    }
    const file = path.join(dir, parts.at(-1));
    const stat = await fs.lstat(file);
    assert(stat.isFile(), `baseline is not a regular file: ${entry.relative}`);
    assert.equal(stat.mode & 0o7777, Number.parseInt(entry.mode, 8) & 0o777, `baseline mode differs: ${entry.relative}`);
    const bytes = await fs.readFile(file);
    assert.equal(createHash("sha256").update(bytes).digest("hex"), entry.sha256, `baseline SHA256 differs: ${entry.relative}`);
    loaded.set(entry.relative, bytes);
  }
  return { files: loaded, provenance: manifest.provenance };
}

module.exports = { loadOfflineBaseline };
