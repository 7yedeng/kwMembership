import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("session_desktop", Path(__file__).with_name("app.py"))
app = importlib.util.module_from_spec(spec)
spec.loader.exec_module(app)


class DesktopContractTests(unittest.TestCase):
    def test_input_is_one_pipe_line_and_never_in_arguments(self):
        raw = '{\n"sessionToken":"synthetic-only-secret",\n"user":{"email":"x@example.invalid"}\n}'
        encoded = app.encode_session(raw)
        self.assertEqual(encoded.count(b"\n"), 1)
        self.assertEqual(json.loads(encoded), json.loads(raw))
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "membership-session"
            browser = Path(directory) / "browser"
            binary.touch(); browser.touch()
            command = app.command(str(binary), str(browser), True, True)
            self.assertNotIn("synthetic-only-secret", " ".join(command))
            self.assertIn("--gui-protocol", command)
            self.assertIn("--show-browser", command)
            self.assertIn("--hold-open", command)

    def test_inspect_does_not_require_browser(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "membership-session"
            binary.touch()
            command = app.command(str(binary), "", False, True)
            self.assertEqual(command[1:], ["--gui-protocol", "--inspect"])

    def test_report_allowlist_discards_material_and_raw_errors(self):
        report = app.safe_report(json.dumps({"code":"SESSION_VERIFIED", "identityMatch":True,
            "sessionToken":"synthetic-only-secret", "email":"private@example.invalid",
            "browserOpened":"true", "targetFingerprint":"not-a-fingerprint"}).encode())
        self.assertEqual(report, {"code":"SESSION_VERIFIED", "identityMatch":True})
        self.assertEqual(app.safe_report(b"fatal raw secret"), {"code":"SESSION_UNEXPECTED"})
        self.assertEqual(app.safe_report(b'{"code":"SESSION_SECRET value"}'), {"code":"SESSION_UNEXPECTED"})

    def test_invalid_input_does_not_start_process(self):
        for raw in ("", "[]", "broken", "x" * (app.MAX_SESSION_BYTES + 1)):
            with self.assertRaises(app.InputError):
                app.encode_session(raw)

    def test_complete_requires_exit_code_and_all_evidence(self):
        report = {"code": "SESSION_VERIFIED", "remoteVerified": True, "identityMatch": True,
                  "cookieReadbackVerified": True, "profileRemoved": True}
        self.assertIn("已通过真实身份核验", app.completion_message(report, 0))
        self.assertIn("未完成", app.completion_message(report, 1))
        for key in ("remoteVerified", "identityMatch", "cookieReadbackVerified", "profileRemoved"):
            missing = dict(report)
            missing.pop(key)
            self.assertIn("未完成", app.completion_message(missing, 0))

    def test_cancel_after_identity_match_is_not_normal_completion(self):
        report = {"code": "SESSION_CANCELLED", "remoteVerified": True, "identityMatch": True,
                  "cookieReadbackVerified": True, "profileRemoved": True}
        message = app.completion_message(report, 1)
        self.assertIn("任务已取消", message)
        self.assertIn("取消前曾核对到指定账号", message)
        self.assertNotIn("已通过真实身份核验", message)
        report["code"] = "SESSION_PROFILE_CLEANUP_FAILED"
        report["profileRemoved"] = False
        self.assertIn("清理失败", app.completion_message(report, 1))


if __name__ == "__main__":
    unittest.main()
