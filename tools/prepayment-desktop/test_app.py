import copy
import importlib.util
import io
import json
from pathlib import Path
import queue
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("prepayment_desktop", Path(__file__).with_name("app.py"))
app = importlib.util.module_from_spec(spec)
spec.loader.exec_module(app)


def settings():
    return {"platformKey": "zovo-fixture", "cardId": "7", "appId": "fixture-app", "appSecret": "synthetic-api-secret", "min": "1000.00", "max": "1200.00"}


def ready_report():
    return {
        "code": "PREPAYMENT_REVIEW_READY", "inputValid": True, "cookieReadbackVerified": True,
        "identityMatch": True, "billingVerified": True, "fieldsVerified": True, "readyForConfirmation": True,
        "browserOpened": True, "browserClosed": True, "paymentAllowed": False, "submitActivated": False, "submissionBlocked": False, "submissionFenceVerified": True,
        "fundingActionsTaken": 0, "targetFingerprint": "0123456789abcdef", "evidenceSource": "visible_dom", "countrySource": "requested_checkout_market",
        "quote": {"plan": "plus", "country": "PH", "currency": "PHP", "minorUnitScale": 2,
                  "subtotalMinor": 98214, "taxMinor": 11786, "discountMinor": 0, "totalMinor": 110000,
                  "taxIncluded": False, "budgetMinMinor": 100000, "budgetMaxMinor": 120000},
        "card": {"cardBindingVerified": True, "last4": "4242", "availableAmount": "30", "currency": "",
                 "balanceCurrencyKnown": False, "balanceComparable": False, "merchantRestrictionsKnown": False, "billingSource": "provider"},
        "unconfirmed": ["card_balance_currency", "card_balance_sufficiency", "merchant_restrictions"],
    }


class RecordingInput(io.BytesIO):
    def __init__(self):
        super().__init__()
        self.writes = []

    def write(self, value):
        self.writes.append(value)
        return super().write(value)


class FakeProcess:
    def __init__(self, output):
        self.stdin = RecordingInput()
        self.stdout = io.BytesIO(output)
        self.stderr = io.BytesIO(b"raw synthetic-api-secret must not display\nPREPARATION_STAGE reviewing_quote\n")

    def wait(self):
        return 0

    def poll(self):
        return 0


class DesktopContractTests(unittest.TestCase):
    def test_input_is_one_private_line_with_bound_optional_billing(self):
        session = '{\n"sessionToken":"synthetic-session-secret", "user":{"email":"owner@example.invalid"}\n}'
        encoded = app.encode_input(session, settings(), {"name": "Fixture Owner", "country": "US"})
        self.assertEqual(encoded.count(b"\n"), 1)
        value = json.loads(encoded)
        self.assertEqual(value["target"], {"plan": "plus", "country": "PH", "currency": "PHP"})
        self.assertEqual(value["billing"]["platformKey"], value["card"]["platformKey"])
        self.assertEqual(value["billing"]["cardId"], value["card"]["cardId"])
        self.assertEqual(value["billing"]["address"]["country"], "US")
        self.assertNotIn("fund", value)

    def test_unknown_price_and_card_id_are_not_generated(self):
        for key, value in (("min", ""), ("max", ""), ("cardId", ""), ("cardId", "0"), ("cardId", "9223372036854775808")):
            changed = settings(); changed[key] = value
            with self.assertRaises(app.InputError):
                app.encode_input("{}", changed)
        for session in ("", "[]", "broken", '{"user":{},"USER":{}}'):
            with self.assertRaises(app.InputError):
                app.encode_input(session, settings())

    def test_check_has_no_browser_and_run_arguments_never_contain_material(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "membership-prepare"; binary.touch()
            chrome = Path(directory) / "chrome"; chrome.touch()
            self.assertEqual(app.command(str(binary), "", False, True)[1:], ["--gui-protocol", "--check"])
            args = app.command(str(binary), str(chrome), True, True)
            self.assertIn("--show-browser", args)
            self.assertIn("--hold-open", args)
            for secret in ("synthetic-api", "sessionToken", "appSecret", "cardId"):
                self.assertNotIn(secret, " ".join(args))
            for option in ("--pay", "--submit", "--fund", "--open-card"):
                self.assertNotIn(option, args)

    def test_report_allowlist_removes_raw_material_and_unknown_codes(self):
        report = ready_report()
        report.update({"session": "synthetic-session-secret", "appSecret": "synthetic-api-secret", "url": "https://secret.invalid"})
        report["card"].update({"number": "4242424242424242", "cvv": "123", "billingName": "Private Name"})
        clean = app.safe_report(report)
        rendered = json.dumps(clean)
        for marker in ("synthetic-", "secret.invalid", "424242424242", "Private Name", '"cvv"'):
            self.assertNotIn(marker, rendered)
        self.assertEqual(app.safe_report({"code": "PREPARATION_SECRETKEYVALUE"}), {"code": "PREPARATION_REPORT_INVALID"})
        self.assertEqual(app.safe_event(b"raw credential"), ("invalid", {"code": "PREPARATION_REPORT_INVALID"}))

    def test_unknown_balance_currency_remains_explicitly_unconfirmed(self):
        report = app.safe_report(ready_report())
        self.assertTrue(app.review_complete(report, final=True))
        self.assertFalse(report["card"]["balanceCurrencyKnown"])
        self.assertFalse(report["card"]["balanceComparable"])
        self.assertNotIn("balanceSufficient", report["card"])
        text = app.review_text(report)
        self.assertIn("币种未确认", text)
        self.assertIn("卡余额是否足以付款尚未确认", text)
        self.assertNotIn("USD", text)

    def test_ready_requires_all_flags_and_checked_arithmetic(self):
        good = ready_report()
        self.assertTrue(app.review_complete(good, final=True))
        for key in ("inputValid", "cookieReadbackVerified", "identityMatch", "billingVerified", "fieldsVerified", "readyForConfirmation", "browserOpened", "browserClosed", "submissionFenceVerified"):
            report = copy.deepcopy(good); report[key] = False
            self.assertFalse(app.review_complete(report, final=True), key)
        for key in ("paymentAllowed", "submitActivated", "submissionBlocked"):
            report = copy.deepcopy(good); report[key] = True
            self.assertFalse(app.review_complete(report, final=True), key)
        for field, value in (("totalMinor", 110001), ("budgetMinMinor", 120001), ("minorUnitScale", 3)):
            report = copy.deepcopy(good); report["quote"][field] = value
            self.assertFalse(app.review_complete(report, final=True))
        report = copy.deepcopy(good); report["card"]["cardBindingVerified"] = False
        self.assertFalse(app.review_complete(report, final=True))
        report = copy.deepcopy(good); report["missingFacts"] = ["arbitrary-unrecognized-fact"]
        self.assertFalse(app.review_complete(app.safe_report(report), final=True))

    def test_completion_requires_successful_exit_and_cleanup(self):
        report = ready_report()
        self.assertIn("付款前准备完成", app.completion_message(report, 0))
        self.assertIn("尚未完成", app.completion_message(report, 1))
        report["browserClosed"] = False
        self.assertIn("尚未完成", app.completion_message(report, 0))
        report["code"] = "PREPAYMENT_CANCELLED"
        self.assertIn("已取消", app.completion_message(report, 1))
        self.assertNotIn("准备完成", app.completion_message(report, 1))

    def test_worker_delivers_ready_before_final_and_only_writes_private_stdin(self):
        ready = ready_report(); ready["browserClosed"] = False
        output = b"".join(json.dumps({"event": event, "report": report}).encode() + b"\n"
                          for event, report in (("ready", ready), ("result", ready_report())))
        process = FakeProcess(output)
        application = app.Application.__new__(app.Application)
        application.events = queue.Queue()
        payload = b'{"session":"synthetic-session-secret","appSecret":"synthetic-api-secret"}\n'
        with patch.object(app.subprocess, "Popen", return_value=process) as spawn:
            application.worker(["membership-prepare", "--gui-protocol", "--run"], payload)
        self.assertEqual(process.stdin.writes, [payload])
        self.assertFalse(spawn.call_args.kwargs["shell"])
        events = []
        while not application.events.empty():
            events.append(application.events.get_nowait())
        kinds = [kind for kind, _ in events]
        self.assertLess(kinds.index("ready"), kinds.index("done"))
        rendered = json.dumps([value for kind, value in events if kind != "started"])
        self.assertNotIn("synthetic-api", rendered)
        self.assertNotIn("synthetic-session", rendered)

    def test_cancel_is_only_control_message_and_is_idempotent(self):
        application = app.Application.__new__(app.Application)
        application.process = FakeProcess(b"")
        application.cancel_sent = False
        application.send_cancel(); application.send_cancel()
        self.assertEqual(application.process.stdin.writes, [b"cancel\n"])


if __name__ == "__main__":
    unittest.main()
