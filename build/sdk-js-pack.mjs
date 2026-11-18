import { execFileSync } from "node:child_process";
import { constants } from "node:fs";
import { copyFile, cp, mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

export function releaseVersion(value) {
  if (typeof value !== "string") throw new Error("VERSION must be a SemVer application release");
  const version = value.replace(/^v/, "");
  const number = "(?:0|[1-9][0-9]*)";
  const identifier = "(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)";
  const semver = new RegExp(`^${number}\\.${number}\\.${number}(?:-${identifier}(?:\\.${identifier})*)?(?:\\+[0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*)?$`);
  if (semver.exec(version)?.[0] !== version) throw new Error("VERSION must be a SemVer application release (optional leading v)");
  return version;
}

export function packReceipt(metadata, name, version) {
  let receipt;
  if (Array.isArray(metadata) && metadata.length === 1) {
    receipt = metadata[0];
  } else if (metadata && typeof metadata === "object" && Object.keys(metadata).length === 1 && Object.hasOwn(metadata, name)) {
    receipt = metadata[name];
  }
  if (receipt?.name !== name || receipt.version !== version) {
    throw new Error("npm package identity differs from the requested application release");
  }
  return receipt;
}

export async function packSDK({ version: input, output }) {
  const version = releaseVersion(input);
  if (typeof output !== "string" || output.length === 0) throw new Error("an output directory is required");
  const destination = resolve(output);
  const scratch = await mkdtemp(join(tmpdir(), "magpie-sdk-pack-"));
  try {
    const stage = join(scratch, "sdk");
    await cp(fileURLToPath(new URL("../sdk/js", import.meta.url)), stage, { recursive: true });
    const manifestPath = join(stage, "package.json");
    const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
    manifest.version = version;
    await writeFile(manifestPath, JSON.stringify(manifest, null, 2) + "\n");
    const npm = process.platform === "win32"
      ? join(dirname(process.execPath), "node_modules/npm/bin/npm-cli.js")
      : await realpath(join(dirname(process.execPath), "npm"));
    const metadata = JSON.parse(execFileSync(process.execPath, [
      npm, "pack", "--offline", "--ignore-scripts", "--json", "--pack-destination", scratch,
    ], { cwd: stage, encoding: "utf8", timeout: 120_000, maxBuffer: 4 * 1024 * 1024 }));
    const receipt = packReceipt(metadata, manifest.name, version);
    const filename = receipt.filename;
    if (typeof filename !== "string" || !/^[A-Za-z0-9._+-]+\.tgz$/.test(filename)) {
      throw new Error("unexpected npm package filename");
    }
    await mkdir(destination, { recursive: true });
    const artifact = join(destination, filename);
    await copyFile(join(scratch, filename), artifact, constants.COPYFILE_EXCL);
    return { ...receipt, artifact };
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    const args = process.argv.slice(2);
    if (args.length !== 4 || args[0] !== "--version" || args[2] !== "--output") {
      throw new Error("usage: node build/sdk-js-pack.mjs --version VERSION --output DIRECTORY");
    }
    console.log(JSON.stringify(await packSDK({ version: args[1], output: args[3] })));
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
