"""Build the native controller, external Go service and static UI offline."""

import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess


def build(destination, version):
    source = Path(__file__).resolve().parent
    repository = source.parents[2]
    platform = json.loads(subprocess.check_output(
        ["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED"],
        cwd=repository, text=True,
    ))
    if platform["CGO_ENABLED"] != "1":
        raise SystemExit("hybrid native builds require CGO_ENABLED=1 and a C compiler")
    if not re.fullmatch(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?", version):
        raise SystemExit("--version must be a semantic version")
    destination = Path(destination).resolve()
    # A distribution must be separate from the source tree: the installer copies
    # every file. Refuse a reused destination rather than ship stale payloads.
    if destination.exists() and any(destination.iterdir()):
        raise SystemExit("distribution directory must be empty")
    suffix = {"darwin": ".dylib", "windows": ".dll"}.get(platform["GOOS"], ".so")
    manifest = json.loads((source / "magpie-plugin.json").read_text())
    target = platform["GOOS"] + "/" + platform["GOARCH"]
    if target not in manifest["compatibility"]["platforms"]:
        raise SystemExit("unsupported hybrid example platform: " + target)
    manifest["version"] = version
    manifest["entry"] = "lib/primary" + suffix
    if platform["GOOS"] == "windows":
        manifest["services"][0]["entry"] += ".exe"
    manifest["compatibility"]["platforms"] = [target]
    ldflags = "-X github.com/yetone/magpie/examples/extensions/hybrid-go.Version=" + version
    for package, entry, flags in (
        ("primary", manifest["entry"], ["-buildmode=c-shared"]),
        ("backend", manifest["services"][0]["entry"], []),
    ):
        output = destination / entry
        output.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run([
            "go", "build", "-trimpath", *flags, "-ldflags", ldflags,
            "-o", str(output), "./examples/extensions/hybrid-go/" + package,
        ], cwd=repository, check=True)
        output.with_suffix(".h").unlink(missing_ok=True)
    # tsc is an author tool only. There is no package download or Bun host.
    ui = destination / "ui"
    ui.mkdir()
    subprocess.run([
        "tsc", "--strict", "--target", "ES2020", "--module", "ES2020",
        "--lib", "ES2020,DOM", "--outDir", str(ui),
        str(source / "ui" / "app.ts"),
    ], cwd=repository, check=True)
    shutil.copyfile(source / "ui" / "index.html", ui / "index.html")
    metadata = json.loads((source / "package.json").read_text())
    metadata["version"] = version
    (destination / "package.json").write_text(json.dumps(metadata, indent=2) + "\n")
    (destination / "magpie-plugin.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Built hybrid {version} for {platform['GOOS']}/{platform['GOARCH']}: {destination}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination")
    parser.add_argument("--version", default="1.0.0")
    args = parser.parse_args()
    build(args.destination, args.version)
