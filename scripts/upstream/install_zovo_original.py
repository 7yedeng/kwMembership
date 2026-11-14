"""Install and inspect the pinned Zovo automation package without running checkout."""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

COMMIT = "19f41ef347cdb3cce52fde0f2b43e12f70dc6ebe"
GIT = "D:/dev/Tools/Git/cmd/git.exe"
NODE = "D:/dev/Tools/Node.js/node.exe"
NPM = "D:/dev/Tools/Node.js/node_modules/npm/bin/npm-cli.js"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    args = parser.parse_args()
    root = args.root.resolve()
    source = root / "source"
    reports = root / "reports"
    reports.mkdir(parents=True, exist_ok=True)
    (root / "tmp").mkdir(exist_ok=True)
    env = os.environ.copy()
    env.update(TEMP=str(root / "tmp"), TMP=str(root / "tmp"), PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD="1")
    report = {"checkedAt": datetime.now(timezone.utc).isoformat(),
              "upstream": "https://github.com/zovocard/zovo_card_cdk_auto", "expectedCommit": COMMIT,
              "realWebsiteAccessed": False, "credentialsProvided": False, "paymentExecuted": False}

    def run(name: str, command: list[str], cwd: Path, expected_code: int = 0) -> subprocess.CompletedProcess[str]:
        result = subprocess.run(command, cwd=cwd, env=env, text=True, encoding="utf-8",
                                errors="replace", capture_output=True, timeout=240)
        (reports / f"{name}.log").write_text(result.stdout + result.stderr, encoding="utf-8")
        if result.returncode != expected_code:
            raise RuntimeError(f"{name}: exit {result.returncode}; inspect reports/{name}.log")
        return result

    try:
        if not source.exists():
            run("clone", [GIT, "-c", "core.autocrlf=false", "clone", "--no-checkout", "--single-branch",
                          "--branch", "master", "https://github.com/zovocard/zovo_card_cdk_auto.git", str(source)], root)
            run("checkout", [GIT, "checkout", "--detach", COMMIT], source)
        report["sourceCommit"] = run("commit", [GIT, "rev-parse", "HEAD"], source).stdout.strip()
        if report["sourceCommit"] != COMMIT:
            raise RuntimeError("existing checkout is not the pinned upstream commit")
        if run("status-before", [GIT, "status", "--porcelain=v1"], source).stdout.strip():
            raise RuntimeError("existing source has changes; refusing reinstall")
        automation = source / "automation"
        lock_before = hashlib.sha256((automation / "package-lock.json").read_bytes()).hexdigest()
        report["node"] = run("node-version", [NODE, "--version"], automation).stdout.strip()
        run("npm-ci", [NODE, NPM, "ci", "--no-audit", "--no-fund"], automation)
        run("npm-ls", [NODE, NPM, "ls", "--depth=0"], automation)
        report["playwright"] = run("playwright-version", [NODE, "node_modules/playwright/cli.js", "--version"], automation).stdout.strip()
        files = sorted(p for p in automation.rglob("*.js") if "node_modules" not in p.parts)
        for index, path in enumerate(files):
            run(f"syntax-{index}", [NODE, "--check", str(path)], automation)
        report["syntaxChecks"] = {"passed": len(files), "files": [str(p.relative_to(automation)).replace("\\", "/") for p in files]}
        missing = run("missing-input", [NODE, "fill-checkout.js"], automation, expected_code=1)
        if "--session" not in missing.stderr or "--pay-url" not in missing.stderr:
            raise RuntimeError("unexpected missing-input result")
        report["missingInputCheck"] = {"exitCode": 1, "status": "EXPECTED_STOP_BEFORE_BROWSER"}
        smoke_js = """const {chromium}=require('playwright');
        (async()=>{const b=await chromium.launch({channel:'chrome',headless:true});
        try{const c=await b.newContext();await c.route('**/*',r=>r.abort());const p=await c.newPage();
        await p.goto('about:blank');if(p.url()!=='about:blank'||(await c.cookies()).length)throw Error('unexpected state');
        console.log(JSON.stringify({passed:true,page:p.url(),version:b.version(),channel:'chrome',kind:'supplemental runtime check'}));
        await c.close();}finally{await b.close();}})().catch(e=>{console.error(e.message);process.exit(1)});"""
        smoke = run("browser-blank", [NODE, "-e", smoke_js], automation)
        report["browserSmoke"] = json.loads(smoke.stdout)
        report["lockSha256"] = hashlib.sha256((automation / "package-lock.json").read_bytes()).hexdigest()
        report["lockUnchanged"] = lock_before == report["lockSha256"]
        report["sourceClean"] = not run("status-after", [GIT, "status", "--porcelain=v1"], source).stdout.strip()
        if not report["sourceClean"] or not report["lockUnchanged"]:
            raise RuntimeError("upstream source or lockfile changed")
        report["runtimeStatus"] = "PASSED"
        report["businessValidation"] = "NOT_RUN"
    except Exception as exc:
        report["runtimeStatus"] = "FAILED"
        report["error"] = str(exc)
    (reports / "installation.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=True, indent=2))
    return 0 if report["runtimeStatus"] == "PASSED" else 1


if __name__ == "__main__":
    raise SystemExit(main())
