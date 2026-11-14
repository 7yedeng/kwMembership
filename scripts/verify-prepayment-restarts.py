#!/usr/bin/env python3
"""Run local-only Chromium restart acceptance and emit a redacted summary.

Exit 0: all three real browser lifecycles passed.
Exit 1: an assertion/runtime failure (or browser missing in required mode).
Exit 2: optional browser unavailable; offline checks alone are not acceptance.
"""
from __future__ import annotations

import argparse
from datetime import datetime, timezone
import importlib
import json
import os
from pathlib import Path
import sys
import unittest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--require-browser", action="store_true", help="fail instead of skipping an unavailable browser")
    parser.add_argument("--output", type=Path, help="optional redacted JSON result path; no credentials are read")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    sys.path.insert(0, str(root))
    if args.require_browser:
        os.environ["KWMEMBERSHIP_REQUIRE_BROWSER_TESTS"] = "1"
    module = importlib.import_module("python_executor.tests.test_browser_restart")
    suite = unittest.defaultTestLoader.loadTestsFromModule(module)
    result = unittest.TextTestRunner(stream=sys.stderr, verbosity=2).run(suite)
    complete = result.wasSuccessful() and len(module.RESTART_EVIDENCE) == module.EXPECTED_LIFECYCLES
    known_blockers = ("PLAYWRIGHT_NOT_INSTALLED", "CHROMIUM_NOT_INSTALLED", "CHROMIUM_UNIX_SOCKET_DENIED",
                      "CHROMIUM_SYSTEM_DEPENDENCIES_MISSING", "CHROMIUM_RESTART_UNEXPECTED_START_FAILURE")
    diagnostics = [message for _, message in result.skipped + result.errors]
    summary = {
        "schemaVersion": 1, "scope": "synthetic-browser-restart-acceptance",
        "recordedAt": datetime.now(timezone.utc).isoformat(),
        "status": "passed" if complete else ("incomplete" if result.wasSuccessful() else "failed"),
        "expectedLifecycles": module.EXPECTED_LIFECYCLES, "completedLifecycles": len(module.RESTART_EVIDENCE),
        "testsRun": result.testsRun, "skipped": len(result.skipped),
        "failures": len(result.failures), "errors": len(result.errors),
        "blockerCodes": [code for code in known_blockers if any(code in message for message in diagnostics)],
        "productionOriginVerified": False, "realAccountVerified": False,
        "liveBillingVerified": False, "paymentSubmitted": False,
        "lifecycles": module.RESTART_EVIDENCE,
    }
    encoded = json.dumps(summary, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded, encoding="utf-8")
    sys.stdout.write(encoded)
    if complete:
        return 0
    return 2 if result.wasSuccessful() else 1


if __name__ == "__main__":
    raise SystemExit(main())
