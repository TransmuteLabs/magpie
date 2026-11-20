import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, readdir, realpath, mkdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { packReceipt, releaseVersion } from "../../../build/sdk-js-pack.mjs";
import { CONTRACT } from "../index.js";

const root = fileURLToPath(new URL("../../../", import.meta.url));
const sdk = join(root, "sdk/js");

async function snapshot(directory) {
  const files = {};
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) Object.assign(files, await snapshot(path));
    else files[path] = createHash("sha256").update(await readFile(path)).digest("hex");
  }
  return files;
}

test("release versions normalize tags without changing wire revision semantics", () => {
  for (const version of ["0.1.90001", "1.2.3-alpha.0", "1.2.3+build.01", "1.2.3-0.a-1+build"]) {
    assert.equal(releaseVersion(version), version);
    assert.equal(releaseVersion("v" + version), version);
  }
  for (const version of [undefined, "", "dev", "31dc123", "1.12", "1.2.3.4", "01.2.3", "1.02.3",
    "1.2.03", "1.2.3-01", "1.2.3-a.01", "1.2.3-", "1.2.3+", "1.2.3\n", " 1.2.3",
    "../1.2.3", "1.2.3;touch marker", "vv1.2.3"]) {
    assert.throws(() => releaseVersion(version), /VERSION must be a SemVer/);
  }
});

test("pack receipts identify exactly one SDK release in either npm format", () => {
  const name = "@magpie/extension-sdk";
  const version = "0.1.90001";
  const receipt = { name, version, filename: "magpie-extension-sdk-0.1.90001.tgz" };
  assert.deepEqual(packReceipt([receipt], name, version), receipt);
  assert.deepEqual(packReceipt({ [name]: receipt }, name, version), receipt);
  for (const invalid of [
    null, receipt, [null], [receipt, receipt], { wrong: receipt },
    { [name]: receipt, another: receipt },
    { [name]: { ...receipt, version: "0.1.90002" } },
    { [name]: { ...receipt, name: "other-package" } },
  ]) {
    assert.throws(() => packReceipt(invalid, name, version), /package identity differs/);
  }
});

test("Make packs two application releases with real offline installable SDK exports", { timeout: 120_000 }, async context => {
  const node = execFileSync("node", ["--print", "process.execPath"], { encoding: "utf8", timeout: 10_000 }).trim();
  const npm = process.platform === "win32"
    ? join(dirname(node), "node_modules/npm/bin/npm-cli.js")
    : await realpath(join(dirname(node), "npm"));
  const directory = await mkdtemp(join(tmpdir(), "magpie-sdk-release-test-"));
  context.after(() => rm(directory, { recursive: true, force: true }));
  const before = await snapshot(sdk);
  const manifest = JSON.parse(await readFile(join(sdk, "package.json"), "utf8"));
  assert.equal(manifest.version, "0.0.0-alpha.0");
  for (const version of ["0.1.90001", "0.1.90002"]) {
    const output = join(directory, version);
    const build = execFileSync("make", [
      "--no-print-directory", "sdk-js-pack", "VERSION=v" + version, "SDK_JS_DIST=" + output,
    ], { cwd: root, encoding: "utf8", timeout: 30_000, maxBuffer: 4 * 1024 * 1024 });
    const packed = JSON.parse(build.trim().split("\n").at(-1));
    assert.equal(packed.name, manifest.name);
    assert.equal(packed.version, version);
    assert.deepEqual(await readdir(output), [packed.filename]);
    const consumer = join(directory, "consumer-" + version);
    await mkdir(consumer);
    await writeFile(join(consumer, "package.json"), JSON.stringify({ private: true, type: "module" }));
    execFileSync(node, [
      npm, "install", "--offline", "--ignore-scripts", "--no-audit", "--no-fund", "--package-lock=false", packed.artifact,
    ], { cwd: consumer, encoding: "utf8", timeout: 30_000 });
    const installed = JSON.parse(await readFile(join(consumer, "node_modules/@magpie/extension-sdk/package.json"), "utf8"));
    assert.equal(installed.version, version);
    assert.deepEqual(installed.exports, manifest.exports);
    for (const entry of Object.values(installed.exports)) {
      assert.ok((await readFile(resolve(consumer, "node_modules/@magpie/extension-sdk", entry.types))).length > 0);
    }
    const imported = JSON.parse(execFileSync(node, ["--input-type=module", "-e", `
      import { CONTRACT, serve } from "@magpie/extension-sdk";
      import { createAuthLinkPresentation } from "@magpie/extension-sdk/ui";
      console.log(JSON.stringify({ wire: CONTRACT.version, serve: typeof serve, ui: typeof createAuthLinkPresentation }));
    `], { cwd: consumer, encoding: "utf8", timeout: 10_000 }));
    assert.deepEqual(imported, { wire: CONTRACT.version, serve: "function", ui: "function" });
    const artifactBefore = createHash("sha256").update(await readFile(packed.artifact)).digest("hex");
    const repeated = spawnSync("make", [
      "--no-print-directory", "sdk-js-pack", "VERSION=v" + version, "SDK_JS_DIST=" + output,
    ], { cwd: root, encoding: "utf8", timeout: 30_000 });
    assert.equal(repeated.error, undefined);
    assert.notEqual(repeated.status, 0);
    assert.match(repeated.stderr, /EEXIST/);
    assert.equal(createHash("sha256").update(await readFile(packed.artifact)).digest("hex"), artifactBefore);
    console.log(JSON.stringify({ fixtureAppVersion: version, packageVersion: installed.version, wire: imported.wire, artifactSHA256: artifactBefore }));
  }
  const refusedOutput = join(directory, "invalid-version");
  const refused = spawnSync(node, [
    join(root, "build/sdk-js-pack.mjs"), "--version", "1.2.3-01", "--output", refusedOutput,
  ], { encoding: "utf8", timeout: 10_000 });
  assert.equal(refused.error, undefined);
  assert.equal(refused.status, 1);
  assert.match(refused.stderr, /VERSION must be a SemVer/);
  await assert.rejects(readdir(refusedOutput), { code: "ENOENT" });
  assert.deepEqual(await snapshot(sdk), before);
});
