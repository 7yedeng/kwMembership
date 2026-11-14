import copy
import importlib.util
import io
import json
from pathlib import Path
import queue
import tempfile
from types import SimpleNamespace
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


def eligibility_failure_report():
    return {
        "code": "PREPAYMENT_ELIGIBILITY_UNCONFIRMED", "stage": "opening_checkout", "inputValid": True,
        "cookieReadbackVerified": True, "identityMatch": True, "browserOpened": True, "browserClosed": True,
        "paymentAllowed": False, "submitActivated": False, "fundingActionsTaken": 0,
        "checkoutDiagnostic": {
            "phase": "account_lookup", "reason": "account_http_rejected", "source": "none",
            "sessionHttpStatus": 200, "accountHttpStatus": 403, "subscriptionHttpStatus": 0, "checkoutHttpStatus": 0,
            "accountIdPresent": True, "accountBinding": "session", "plan": "unknown",
            "activeSubscription": "unknown", "checkoutRequested": False,
        },
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

    def test_failure_preserves_stage_and_does_not_claim_a_whitelist_diagnosis(self):
        report = app.safe_report({"code": "PREPARATION_CARD_ACCESS_DENIED", "stage": "resolving_billing",
                                  "browserOpened": True, "browserClosed": True, "raw": "synthetic-api-secret"})
        self.assertEqual(report["stage"], "resolving_billing")
        text = app.review_text(report)
        self.assertIn("HTTP 403", text)
        self.assertIn("不能确定", text)
        self.assertIn("失败后已立即关闭", text)
        self.assertNotIn("尚未查询 Zovo", text)
        self.assertNotIn("synthetic-api-secret", text)
        early = app.safe_report({"code": "SESSION_NOT_LOGGED_IN", "stage": "restoring_session"})
        self.assertIn("尚未查询 Zovo", app.review_text(early))
        unknown = app.safe_report({"code": "PREPARATION_CARD_UNAVAILABLE", "stage": "synthetic-api-secret"})
        self.assertNotIn("stage", unknown)
        for suffix in ("AUTH_FAILED", "ACCESS_DENIED", "RATE_LIMITED", "TIMEOUT", "OPERATION_REJECTED"):
            code = "PREPARATION_CARD_" + suffix
            self.assertEqual(app.safe_report({"code": code})["code"], code)

    def test_saved_diagnostic_excludes_account_card_quote_and_credentials(self):
        report = ready_report()
        report.update({"stage": "browser_closed", "session": "synthetic-session-secret", "appSecret": "synthetic-api-secret"})
        with tempfile.TemporaryDirectory() as directory:
            target = app.save_diagnostic(report, "review_ready", Path(directory))
            saved = json.loads(target.read_text(encoding="utf-8"))
            self.assertEqual(saved["stage"], "browser_closed")
            self.assertEqual(saved["lastProgressStage"], "review_ready")
            self.assertEqual(saved["fundingActionsTaken"], 0)
            self.assertIn("savedAtUTC", saved)
            for key in ("targetFingerprint", "card", "quote", "session", "appSecret"):
                self.assertNotIn(key, saved)
            for marker in ("synthetic-", "0123456789abcdef", "4242", "98214"):
                self.assertNotIn(marker, json.dumps(saved))
            self.assertEqual([item.name for item in Path(directory).iterdir()], ["last-diagnostic.json"])
        self.assertNotIn("lastProgressStage", app.diagnostic_report(report, "synthetic-api-secret"))

    def test_diagnostic_write_failure_keeps_previous_result(self):
        with tempfile.TemporaryDirectory() as directory:
            folder = Path(directory)
            target = app.save_diagnostic({"code": "SESSION_NOT_LOGGED_IN", "stage": "restoring_session"}, directory=folder)
            before = target.read_bytes()
            with patch.object(app.os, "replace", side_effect=OSError("write denied")):
                with self.assertRaises(OSError):
                    app.save_diagnostic(ready_report(), directory=folder)
            self.assertEqual(target.read_bytes(), before)
            self.assertEqual([item.name for item in folder.iterdir()], ["last-diagnostic.json"])

    def test_eligibility_failure_preserves_query_reason_and_pre_checkout_state(self):
        report = eligibility_failure_report()
        event, clean = app.safe_event(json.dumps({"event": "result", "report": report}).encode())
        self.assertEqual(event, "result")
        self.assertEqual(clean["checkoutDiagnostic"], report["checkoutDiagnostic"])
        text = app.review_text(clean)
        self.assertIn("实时账号查询返回非成功状态", text)
        self.assertIn("账号查询 HTTP：403", text)
        self.assertIn("创建结账请求：尚未发起", text)
        self.assertIn("套餐分类：未确认", text)
        self.assertNotIn("套餐分类：免费", text)
        self.assertIn("尚未查询 Zovo", text)
        created = copy.deepcopy(report)
        created["checkoutDiagnostic"].update({"phase": "complete", "reason": "checkout_created", "plan": "free",
                                             "source": "account_entitlement", "checkoutHttpStatus": 200, "checkoutRequested": True})
        created_text = app.review_text(created)
        self.assertIn("创建结账请求：已发起", created_text)
        self.assertIn("不代表结账页面已打开", created_text)

    def test_checkout_diagnostic_strict_types_and_unknown_values_never_become_free(self):
        valid = eligibility_failure_report()["checkoutDiagnostic"]
        for key in ("phase", "reason", "source", "accountBinding", "plan", "activeSubscription"):
            for invalid in ("synthetic-private-value", None, [], {}):
                with self.subTest(key=key, invalid=invalid):
                    value = copy.deepcopy(valid); value[key] = invalid
                    self.assertNotIn(key, app.safe_checkout_diagnostic(value))
        for key in app.CHECKOUT_HTTP_LABELS:
            for invalid in (-1, 1, 99, 600, 200.5, "200", True):
                value = copy.deepcopy(valid); value[key] = invalid
                self.assertNotIn(key, app.safe_checkout_diagnostic(value))
            for allowed in (0, 100, 200, 599):
                value = copy.deepcopy(valid); value[key] = allowed
                self.assertEqual(app.safe_checkout_diagnostic(value)[key], allowed)
        for key in ("accountIdPresent", "checkoutRequested"):
            for invalid in (0, 1, "false", "true", None, []):
                value = copy.deepcopy(valid); value[key] = invalid
                self.assertNotIn(key, app.safe_checkout_diagnostic(value))
        report = eligibility_failure_report()
        report["checkoutDiagnostic"].update({"plan": "synthetic-private-value", "checkoutRequested": "true"})
        text = app.review_text(report)
        self.assertIn("套餐分类：未确认", text)
        self.assertIn("创建结账请求：未确认", text)
        self.assertNotIn("套餐分类：免费", text)
        self.assertNotIn("创建结账请求：已发起", text)

    def test_alias_conflict_is_distinct_from_ambiguous_mapping_and_http_failure(self):
        report = eligibility_failure_report()
        report["checkoutDiagnostic"].update({"reason": "account_alias_conflict", "accountHttpStatus": 200,
                                             "source": "account_entitlement", "accountBinding": "matched"})
        event, clean = app.safe_event(json.dumps({"event": "result", "report": report}).encode())
        self.assertEqual(event, "result")
        self.assertEqual(clean["checkoutDiagnostic"], report["checkoutDiagnostic"])
        text = app.review_text(clean)
        self.assertIn("同一账号多个索引中的明确套餐或订阅状态不一致", text)
        self.assertIn("账号查询 HTTP：200", text)
        self.assertIn("套餐分类：未确认", text)
        self.assertIn("有效订阅：未确认", text)
        self.assertIn("创建结账请求：尚未发起", text)
        self.assertIn("尚未查询 Zovo", text)
        self.assertFalse(app.review_complete(clean, final=True))
        for unsupported in ("返回非成功状态", "套餐分类：免费", "已有付费套餐或有效订阅", "创建结账请求：已发起"):
            self.assertNotIn(unsupported, text)
        report["checkoutDiagnostic"]["reason"] = "account_ambiguous"
        old_text = app.review_text(report)
        self.assertIn("多个索引本身不代表多个账号", old_text)
        self.assertIn("没有自行切换工作区", old_text)
        self.assertNotIn("明确套餐或订阅状态不一致", old_text)

    def test_alias_diagnostics_keep_original_twelve_fields_in_display_copy_and_file(self):
        report = eligibility_failure_report()
        diagnostic = report["checkoutDiagnostic"]
        original_fields = set(diagnostic)
        private = "synthetic-private-alias-material"
        diagnostic.update({"reason": "account_alias_conflict", "accountHttpStatus": 200,
                           "aliases": [{"key": private, "account": {"account_id": private},
                                        "entitlement": {"raw": private}}],
                           "accountIds": [private], "conflictingPlans": [private], "aliasCount": 2,
                           "defaultAccount": private, "rawResponse": private})
        clean = app.diagnostic_report(report, "opening_checkout")
        self.assertEqual(len(original_fields), 12)
        self.assertEqual(set(clean["checkoutDiagnostic"]), original_fields)
        self.assertEqual(clean["checkoutDiagnostic"]["reason"], "account_alias_conflict")
        self.assertEqual(clean["diagnosticVersion"], "20261114.3")
        writes = []
        application = app.Application.__new__(app.Application)
        application.last_diagnostic = clean
        application.root = SimpleNamespace(clipboard_clear=lambda: writes.clear(), clipboard_append=writes.append)
        application.status = SimpleNamespace(set=lambda value: None)
        application.copy_diagnostic()
        with tempfile.TemporaryDirectory() as directory:
            saved = app.save_diagnostic(report, "opening_checkout", Path(directory)).read_text(encoding="utf-8")
        for serialized in (writes[0], saved):
            self.assertEqual(json.loads(serialized)["checkoutDiagnostic"], clean["checkoutDiagnostic"])
        for output in (app.review_text(report), writes[0], saved):
            for forbidden in (private, '"aliases"', '"accountIds"', '"conflictingPlans"', '"aliasCount"', '"defaultAccount"', '"rawResponse"'):
                self.assertNotIn(forbidden, output)

    def test_nested_checkout_diagnostic_stays_private_in_render_copy_and_saved_file(self):
        report = eligibility_failure_report()
        private = "synthetic-private-checkout-value"
        report["checkoutDiagnostic"].update({"accountId": private, "email": private, "accessToken": private,
                                             "url": "https://private.invalid/" + private, "body": {"secret": private}})
        report.update({"session": private, "appSecret": private, "targetFingerprint": "0123456789abcdef"})
        diagnostic = app.diagnostic_report(report, "opening_checkout")
        self.assertEqual(diagnostic["diagnosticVersion"], "20261114.3")
        self.assertEqual(diagnostic["checkoutDiagnostic"]["reason"], "account_http_rejected")
        rendered = app.review_text(report)
        writes = []
        application = app.Application.__new__(app.Application)
        application.last_diagnostic = diagnostic
        application.root = SimpleNamespace(clipboard_clear=lambda: writes.clear(), clipboard_append=writes.append)
        application.status = SimpleNamespace(set=lambda value: None)
        application.copy_diagnostic()
        self.assertEqual(json.loads(writes[0]), diagnostic)
        with tempfile.TemporaryDirectory() as directory:
            target = app.save_diagnostic(report, "opening_checkout", Path(directory))
            saved = target.read_text(encoding="utf-8")
            self.assertEqual(json.loads(saved)["checkoutDiagnostic"], diagnostic["checkoutDiagnostic"])
        for output in (rendered, writes[0], saved):
            self.assertNotIn(private, output)
            self.assertNotIn("private.invalid", output)
            self.assertNotIn('"accountId"', output)
        for output in (writes[0], saved):
            self.assertNotIn("0123456789abcdef", output)
            self.assertNotIn('"quote"', output)
            self.assertNotIn('"card"', output)

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
