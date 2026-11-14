"""Deployment checks around an unchanged upstream checkout; never runs its worker."""
from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone


EXPECTED = "16ceb97fc15f629c8b59df242e1baee2227e4c77"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    source = root / "source"
    reports = root / "reports"
    reports.mkdir(exist_ok=True)
    report = {
        "checkedAt": datetime.now(timezone.utc).isoformat(),
        "upstream": "https://github.com/varrge/kwMembership",
        "expectedCommit": EXPECTED,
        "platform": platform.platform(),
        "python": platform.python_version(),
        "scope": "upstream unit tests and supplemental about:blank runtime check",
        "realWebsiteAccessed": False,
        "workerStarted": False,
        "paymentExecuted": False,
    }
    env = os.environ.copy()
    for name in list(env):
        if name.startswith("KWMEMBERSHIP_") or name in {"KAWANG_PROJECT_ROOT", "PYTHONPATH", "PYTHONHOME"}:
            env.pop(name, None)

    def run(name: str, command: list[str], expected_code: int = 0) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(command, cwd=source, env=env, text=True,
                                encoding="utf-8", errors="replace", capture_output=True, timeout=120)
        (reports / f"{name}.log").write_text(result.stdout + result.stderr, encoding="utf-8")
        if result.returncode != expected_code:
            raise RuntimeError(f"{name}: exit {result.returncode}; see reports/{name}.log")
        return result

    try:
        git = shutil.which("git")
        if not git:
            raise RuntimeError("git is required for source verification")
        report["sourceCommit"] = run("source-commit", [git, "rev-parse", "HEAD"]).stdout.strip()
        report["trackedFiles"] = len(run("source-files", [git, "ls-files"]).stdout.splitlines())
        report["sourceClean"] = not run("source-status", [git, "status", "--porcelain=v1"]).stdout.strip()
        if report["sourceCommit"] != EXPECTED or not report["sourceClean"]:
            raise RuntimeError("source checkout differs from pinned upstream")
        binary = source / "bin" / "kwmembership-worker"
        binary_data = binary.read_bytes()
        report["workerBinarySha256"] = hashlib.sha256(binary_data).hexdigest()
        report["workerBinaryFormat"] = "PE" if binary_data[:2] == b"MZ" else "other"
        go_version = re.search(rb"go1\.[0-9]+(?:\.[0-9]+)?", binary_data)
        report["embeddedGoVersion"] = go_version.group().decode() if go_version else None
        report["playwright"] = importlib.metadata.version("playwright")
        if report["playwright"] != "1.61.0":
            raise RuntimeError("Playwright must match upstream requirements.txt")
        run("pip-check", [sys.executable, "-m", "pip", "check"])
        test = run("python-upstream-tests", [sys.executable, "-m", "unittest",
                   "python_executor.tests.test_client", "python_executor.tests.test_live", "-v"])
        match = re.search(r"Ran (\d+) tests?", test.stdout + test.stderr)
        if not match or int(match.group(1)) != 8:
            raise RuntimeError("unexpected upstream Python test inventory")
        report["pythonTests"] = {"passed": 8, "failed": 0, "skipped": 0}
        run("python-help", [sys.executable, "-m", "python_executor", "--help"])

        # This is an installation smoke check, not an upstream checkout/form test.
        from playwright.sync_api import sync_playwright
        browsers = root.parent / "kwMembership-playwright-20261114" / "browsers"
        os.environ["PLAYWRIGHT_BROWSERS_PATH"] = str(browsers)
        tmp = root / "tmp"
        tmp.mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="upstream-blank-", dir=tmp) as profile:
            with sync_playwright() as pw:
                context = pw.chromium.launch_persistent_context(
                    profile, headless=True, viewport={"width": 1280, "height": 900},
                    service_workers="block")
                try:
                    context.route("**/*", lambda route: route.abort())
                    page = context.pages[0] if context.pages else context.new_page()
                    page.goto("about:blank")
                    if page.url != "about:blank" or context.cookies():
                        raise RuntimeError("unexpected initial browser state")
                    report["browserSmoke"] = {"passed": True, "page": "about:blank",
                                               "version": context.browser.version,
                                               "kind": "supplemental runtime check"}
                finally:
                    context.close()
        report["browserSmoke"]["profileRemoved"] = not Path(profile).exists()
        companion = root / "kwRedeem"
        if (companion / "package.json").exists():
            report["configuration"] = {"status": "NOT_RUN_EXISTING_COMPANION",
                                       "reason": "offline verifier does not open an existing business database"}
        else:
            env["KWMEMBERSHIP_PROJECT_ROOT"] = str(source)
            env["KAWANG_PROJECT_ROOT"] = str(companion)
            bash = Path("D:/dev/Tools/Git/bin/bash.exe")
            check = run("upstream-configuration-check", [str(bash), "scripts/check.sh"], expected_code=1)
            if "KAWANG_PROJECT_ROOT is not a kwRedeem checkout" not in check.stdout + check.stderr:
                raise RuntimeError("unexpected configuration failure; inspect check log")
            report["configuration"] = {"status": "BLOCKED_MISSING_KWREDEEM", "exitCode": 1,
                                       "databaseCreated": False}
        report["sourceCleanAfterVerification"] = not run("source-status-after", [git, "status", "--porcelain=v1"]).stdout.strip()
        if not report["sourceCleanAfterVerification"]:
            raise RuntimeError("source checkout changed during verification")
        report["runtimeStatus"] = "PASSED"
    except Exception as exc:
        report["runtimeStatus"] = "FAILED"
        report["error"] = str(exc)
    (reports / "runtime-verification.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, indent=2))
    return 0 if report["runtimeStatus"] == "PASSED" else 1


if __name__ == "__main__":
    raise SystemExit(main())
