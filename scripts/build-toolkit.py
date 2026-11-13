#!/usr/bin/env python3
"""Build the isolated account/review tools; never starts a browser or a worker."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import zipfile

ROOT = Path(__file__).resolve().parents[1]
COMMANDS = ("membership-session", "membership-review", "membership-preflight")
TARGETS = {"windows-amd64": ("windows", "amd64"), "linux-amd64": ("linux", "amd64")}
DOCS = ("SESSION_RESTORE.md", "CHECKOUT_REVIEW.md", "CARD_ADAPTERS.md", "SINGLE_FLOW_DEVELOPMENT.md")


def run(arguments: list[str], *, env: dict[str, str] | None = None) -> str:
    completed = subprocess.run(arguments, cwd=ROOT, env=env, check=True,
                               text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    return completed.stdout


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def copy_licenses(go: str, destination: Path, env: dict[str, str]) -> None:
    # Ask the compiler for the actual module graph used by these commands.
    # No credentials, personal files, source checkouts or module caches are bundled.
    raw = run([go, "list", "-deps", "-json", *["./cmd/" + name for name in COMMANDS]], env=env)
    decoder = json.JSONDecoder()
    modules: dict[str, Path] = {}
    while raw.strip():
        raw = raw.lstrip()
        item, end = decoder.raw_decode(raw)
        raw = raw[end:]
        module = item.get("Module") or {}
        if module.get("Main") or not module.get("Dir"):
            continue
        modules[module["Path"] + "@" + module.get("Version", "")] = Path(module["Dir"])
    license_dir = destination / "THIRD_PARTY_LICENSES"
    license_dir.mkdir()
    manifest: list[dict[str, object]] = []
    for name, directory in sorted(modules.items()):
        copied = []
        for candidate in sorted(directory.iterdir()):
            if candidate.is_file() and candidate.name.upper().startswith(("LICENSE", "COPYING", "NOTICE")):
                target = license_dir / (name.replace("/", "_") + "-" + candidate.name)
                shutil.copyfile(candidate, target)
                copied.append(target.name)
        if not copied:
            raise RuntimeError("Missing license notice for dependency: " + name)
        manifest.append({"module": name, "notices": copied})
    (license_dir / "modules.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    goroot = Path(run([go, "env", "GOROOT"], env=env).strip())
    shutil.copyfile(goroot / "LICENSE", license_dir / "Go-LICENSE")


def build(target: str, output: Path, go: str) -> Path:
    folder = output / ("membership-toolkit-" + target)
    if folder.exists():
        raise RuntimeError("Output directory already exists; choose a fresh --output directory")
    (folder / "bin").mkdir(parents=True)
    operating_system, architecture = TARGETS[target]
    environment = dict(os.environ, GOOS=operating_system, GOARCH=architecture,
                       CGO_ENABLED="0", GOTOOLCHAIN="local", GOPROXY="off")
    suffix = ".exe" if operating_system == "windows" else ""
    for command in COMMANDS:
        run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o",
             str(folder / "bin" / (command + suffix)), "./cmd/" + command], env=environment)
    shutil.copytree(ROOT / "tools" / "session-desktop", folder / "tools" / "session-desktop",
                    ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "test_*.py"))
    shutil.copytree(ROOT / "tools" / "userscript", folder / "userscript",
                    ignore=shutil.ignore_patterns("*.zip", "*.test.cjs", "TEST_RESULTS.txt"))
    (folder / "docs").mkdir()
    for filename in DOCS:
        shutil.copyfile(ROOT / "docs" / filename, folder / "docs" / filename)
    shutil.copyfile(ROOT / "docs" / "TOOLKIT_QUICKSTART.md", folder / "START_HERE_zh-CN.md")
    if (ROOT / "LICENSE").exists():
        shutil.copyfile(ROOT / "LICENSE", folder / "LICENSE")
    copy_licenses(go, folder, environment)
    source = {
        "revision": run(["git", "rev-parse", "HEAD"]).strip(),
        "workingTreeDirty": bool(run(["git", "status", "--porcelain"]).strip()),
        "target": target,
        "commands": list(COMMANDS),
        "liveAccountValidated": False,
        "paymentEntryIncluded": False,
    }
    (folder / "SOURCE.json").write_text(json.dumps(source, indent=2) + "\n", encoding="utf-8")
    if operating_system == "windows":
        (folder / "打开会话工具.cmd").write_text(
            '@echo off\r\ncd /d "%~dp0"\r\n'
            'call "tools\\session-desktop\\start.cmd"\r\n', encoding="utf-8", newline="")
    else:
        launch = folder / "open-session-tool.sh"
        launch.write_text('#!/usr/bin/env sh\nset -eu\ncd "$(dirname "$0")"\n'
                          'exec python3 tools/session-desktop/app.py\n', encoding="utf-8")
        launch.chmod(0o755)
    manifest = {str(path.relative_to(folder)): sha256(path)
                for path in sorted(folder.rglob("*")) if path.is_file()}
    (folder / "SHA256SUMS.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    archive = output / (folder.name + ".zip")
    with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as handle:
        for path in sorted(folder.rglob("*")):
            if path.is_file():
                handle.write(path, path.relative_to(output))
    with zipfile.ZipFile(archive) as handle:
        if handle.testzip() is not None:
            raise RuntimeError("Archive verification failed")
    return archive


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--go", default=shutil.which("go"))
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--target", action="append", choices=sorted(TARGETS))
    arguments = parser.parse_args()
    if not arguments.go:
        parser.error("Go is required; supply --go PATH")
    output = arguments.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    try:
        for target in arguments.target or TARGETS:
            archive = build(target, output, arguments.go)
            print(json.dumps({"target": target, "archive": str(archive),
                              "bytes": archive.stat().st_size, "sha256": sha256(archive)}))
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        # Build errors are about source/dependencies, never real account inputs.
        print(str(error), file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError):
            print(error.stderr or "", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
