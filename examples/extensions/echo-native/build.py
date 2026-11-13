"""Build a platform-specific native example distribution without C glue."""

import json
from pathlib import Path
import subprocess
import sys


def build(destination):
    source = Path(__file__).resolve().parent
    repository = source.parents[2]
    platform = json.loads(
        subprocess.check_output(
            ["go", "env", "-json", "GOOS", "GOARCH", "CGO_ENABLED"],
            cwd=repository,
            text=True,
        )
    )
    if platform["CGO_ENABLED"] != "1":
        raise SystemExit("native examples require CGO_ENABLED=1 and a C compiler")
    suffix = {"darwin": ".dylib", "windows": ".dll"}.get(platform["GOOS"], ".so")
    destination = Path(destination).resolve()
    destination.mkdir(parents=True, exist_ok=True)
    entry = "plugin" + suffix
    subprocess.run(
        [
            "go", "build", "-trimpath", "-buildmode=c-shared",
            "-o", str(destination / entry), "./examples/extensions/echo-native",
        ],
        cwd=repository,
        check=True,
    )
    manifest = json.loads((source / "magpie-plugin.json").read_text())
    manifest["entry"] = entry
    (destination / "magpie-plugin.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (destination / "plugin.h").unlink(missing_ok=True)
    print(f"Built {platform['GOOS']}/{platform['GOARCH']}: {destination}")


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("usage: python3 build.py DISTRIBUTION_DIRECTORY")
    build(sys.argv[1])
