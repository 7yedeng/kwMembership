from __future__ import annotations

import unittest
from unittest import mock

from python_executor.client import ExecutorAPIError, ExecutorLease
from python_executor.live import (
    LiveExecutor,
    _allowed_fill_origin,
    _classify,
    _fill_frames,
    _resolve_checkout_entry,
    route_template,
    session_cookies,
    validate_checkout_url,
)


def lease(stage: str = "plus", target_tier: str = "plus") -> ExecutorLease:
    return ExecutorLease(
        execution_id="exec-1",
        executor_id="python-test",
        lease_epoch=1,
        lease_token="opaque",
        hard_deadline_at="2026-08-13T00:05:00Z",
        command_kind="payment",
        stage=stage,
        attempt_no=1,
        target_tier=target_tier,
        adapter_version="python-session-card-checkout-v1",
        price_contract={"MinAmount": 1000, "MaxAmount": 1200},
    )


class LiveContractTest(unittest.TestCase):
    def test_fill_origin_requires_exact_https_origin_without_userinfo(self) -> None:
        for value in (
            "https://chatgpt.com/checkout/test", "https://pay.openai.com/pay/test",
            "https://js.stripe.com/v3/element", "https://PAY.OPENAI.COM:443/pay/test",
        ):
            with self.subTest(value=value):
                self.assertTrue(_allowed_fill_origin(value))
        for value in (
            "https://pay.openai.com.evil.example/", "https://user@pay.openai.com/pay/test",
            "https://pay.openai.com:443@evil.example/", "http://pay.openai.com/pay/test",
            "https://pay.openai.com:8443/pay/test", "https://pay.openai.com:invalid/pay/test",
            "https://pay.openai.com./pay/test", "https://pay.openai.com\n/pay/test",
            "blob:https://pay.openai.com/test", "about:blank", "", None,
        ):
            with self.subTest(value=value):
                self.assertFalse(_allowed_fill_origin(value))

    def test_disallowed_frame_never_receives_payment_material(self) -> None:
        foreign = mock.Mock(url="https://foreign.example/frame")
        allowed = mock.Mock(url="https://pay.openai.com/pay/test")
        allowed.evaluate.return_value = self._filled_result()
        _fill_frames(mock.Mock(frames=[foreign, allowed]), {})
        foreign.evaluate.assert_not_called()
        allowed.evaluate.assert_called_once()

    def test_present_rejected_optional_field_stops_checkout_before_any_action(self) -> None:
        frame = mock.Mock(url="https://pay.openai.com/pay/test")
        frame.evaluate.return_value = self._filled_result(rejected=["billingState"])
        client = mock.Mock()
        facts = {"stateId": "PAYMENT_FINAL_READY", "structuralHash": "fixture-snapshot"}
        with self.assertRaises(ExecutorAPIError) as raised:
            LiveExecutor()._checkout(client, lease(), {}, mock.Mock(frames=[frame]), 0, initial=facts)
        self.assertEqual(raised.exception.code, "PAYMENT_FIELDS_NOT_FILLED")
        client.prepare_action.assert_not_called()
        client.activate_action.assert_not_called()
        client.report_submitted.assert_not_called()

    def test_failed_trusted_frame_cannot_be_hidden_by_another_filled_frame(self) -> None:
        filled = mock.Mock(url="https://pay.openai.com/pay/test")
        filled.evaluate.return_value = self._filled_result()
        unavailable = mock.Mock(url="https://js.stripe.com/v3/element")
        unavailable.evaluate.side_effect = RuntimeError("synthetic-sensitive-field-marker")
        with self.assertRaises(ExecutorAPIError) as raised:
            _fill_frames(mock.Mock(frames=[filled, unavailable]), {})
        self.assertEqual(raised.exception.code, "PAYMENT_FIELDS_NOT_FILLED")
        self.assertNotIn("synthetic-sensitive-field-marker", str(raised.exception))
        self.assertTrue(raised.exception.__suppress_context__)

    def test_refused_or_malformed_fill_report_never_counts_as_filled(self) -> None:
        for result in (
            {"accepted": False, "filled": [], "rejected": []},
            {"accepted": True, "filled": ["cardNumber"]},
            {"accepted": True, "filled": ["unexpected-field"], "rejected": []},
            {"accepted": True, "filled": "cardNumber", "rejected": []},
            None,
        ):
            frame = mock.Mock(url="https://pay.openai.com/pay/test")
            frame.evaluate.return_value = result
            with self.subTest(result=result), self.assertRaises(ExecutorAPIError):
                _fill_frames(mock.Mock(frames=[frame]), {})

    def _filled_result(self, rejected: list[str] | None = None) -> dict[str, object]:
        return {
            "accepted": True,
            "filled": ["cardNumber", "cvc", "expiry", "billingName", "billingCountry", "billingPostal"],
            "rejected": rejected or [],
        }

    def test_chunks_session_cookie_like_go_provider(self) -> None:
        cookies = session_cookies(
            {"sessionToken": "x" * 4400, "expires": "2026-08-14T00:00:00Z"},
            now=1_786_579_200,
        )
        self.assertEqual([cookie["name"] for cookie in cookies], [
            "__Secure-next-auth.session-token.0",
            "__Secure-next-auth.session-token.1",
        ])
        self.assertEqual([len(cookie["value"]) for cookie in cookies], [3936, 464])
        self.assertTrue(all(cookie["httpOnly"] and cookie["secure"] for cookie in cookies))

    def test_checkout_url_allowlist_rejects_credentials_and_foreign_hosts(self) -> None:
        allowed = "https://chatgpt.com/checkout/openai_llc/oaics_safe_123"
        self.assertEqual(validate_checkout_url(allowed), allowed)
        self.assertEqual(route_template(allowed), "/checkout/{id}")
        for value in (
            "https://chatgpt.com.evil.example/checkout/oaics_safe",
            "https://user@chatgpt.com/checkout/oaics_safe",
            "http://chatgpt.com/checkout/oaics_safe",
            "https://chatgpt.com/checkout/openai_other/oaics_safe",
        ):
            with self.subTest(value=value), self.assertRaises(ExecutorAPIError):
                validate_checkout_url(value)

    def test_resolves_only_supported_checkout_response_contracts(self) -> None:
        self.assertEqual(
            _resolve_checkout_entry({
                "responseTag": "custom_checkout_session",
                "checkoutURL": "",
                "processorEntity": "openai_llc",
                "checkoutSessionID": "cs_safe_123",
                "errorKind": "",
            }),
            "https://chatgpt.com/checkout/openai_llc/cs_safe_123",
        )
        with self.assertRaises(ExecutorAPIError) as raised:
            _resolve_checkout_entry({"errorKind": "already_subscribed"})
        self.assertEqual(raised.exception.code, "CHATGPT_ACCOUNT_ALREADY_SUBSCRIBED")

    def test_classifies_plus_final_and_tier_specific_upgrade(self) -> None:
        fields = {
            "cardNumber": True,
            "expiry": True,
            "cvc": True,
            "billingName": True,
            "billingCountry": True,
            "billingPostal": True,
        }
        plus = {
            "origin": "https://pay.openai.com",
            "routeTemplate": "/pay/{id}",
            "plan": "plus",
            "country": "PH",
            "currency": "PHP",
            "displayedAmount": 1100,
            "fields": fields,
            "controls": {"submit": "payment-submit"},
        }
        self.assertEqual(_classify(plus, lease(), "checkout"), "PAYMENT_FINAL_READY")

        selection = {
            "origin": "https://chatgpt.com",
            "routeTemplate": "/settings/subscription",
            "plan": "prolite",
            "country": "PH",
            "currency": "PHP",
            "displayedAmount": 1100,
            "fields": {},
            "controls": {"upgradeX5": "upgrade-x5"},
        }
        self.assertEqual(
            _classify(selection, lease("upgrade", "x5"), "selection"),
            "UPGRADE_SELECTION_READY",
        )
        self.assertEqual(
            _classify(selection, lease("upgrade", "x20"), "selection"),
            "UNKNOWN_PAYMENT_STATE",
        )

    def test_action_click_is_between_activate_and_result(self) -> None:
        calls: list[str] = []

        class Client:
            def prepare_action(self, *_args: object) -> dict[str, str]:
                calls.append("prepare")
                return {"permitId": "permit-1"}

            def activate_action(self, *_args: object) -> dict[str, bool]:
                calls.append("activate")
                return {"activated": True}

            def action_result(self, *_args: object) -> dict[str, bool]:
                calls.append("result")
                return {"continue": True}

        class Locator:
            def count(self) -> int:
                return 1

            def is_visible(self) -> bool:
                return True

            def is_enabled(self) -> bool:
                return True

            def click(self, timeout: int) -> None:
                self.assert_timeout = timeout
                calls.append("click")

        class Frame:
            url = "https://pay.openai.com/pay/test"

            def locator(self, _selector: str) -> Locator:
                return Locator()

        class Page:
            frames = [Frame()]

        result = LiveExecutor()._activate(
            Client(), lease(), Page(), {"stateId": "PAYMENT_FINAL_READY"}, "submit", "payment-submit"
        )
        self.assertTrue(result["continue"])
        self.assertEqual(calls, ["prepare", "activate", "click", "result"])

    def test_dispatched_click_timeout_reports_unknown_without_another_click(self) -> None:
        for report_unavailable in (False, True):
            with self.subTest(report_unavailable=report_unavailable):
                calls: list[str] = []

                class Client:
                    def prepare_action(self, *_args: object) -> dict[str, str]:
                        calls.append("prepare")
                        return {"permitId": "permit-1"}

                    def activate_action(self, *_args: object) -> dict[str, bool]:
                        calls.append("activate")
                        return {"activated": True}

                    def action_result(self, _lease: object, _permit: str, outcome: str) -> dict[str, bool]:
                        calls.append("result:" + outcome)
                        if report_unavailable:
                            raise ExecutorAPIError("EXECUTOR_REQUEST_UNAVAILABLE", 503)
                        return {"continue": False}

                class Locator:
                    def count(self) -> int:
                        return 1

                    def is_visible(self) -> bool:
                        return True

                    def is_enabled(self) -> bool:
                        return True

                    def click(self, timeout: int) -> None:
                        calls.append("click")
                        # Simulate the browser receiving input, then losing
                        # the navigation acknowledgement. A later click could
                        # be a second real payment and must never happen.
                        if calls.count("click") == 1:
                            raise TimeoutError("simulated timeout after dispatch")

                class Frame:
                    url = "https://pay.openai.com/pay/test"

                    def locator(self, _selector: str) -> Locator:
                        return Locator()

                class Page:
                    frames = [Frame(), Frame()]

                with self.assertRaises(ExecutorAPIError) as raised:
                    LiveExecutor()._activate(
                        Client(), lease(), Page(), {"stateId": "PAYMENT_FINAL_READY"}, "submit", "payment-submit"
                    )
                self.assertEqual(raised.exception.code, "PERMIT_ACTIVATION_UNCERTAIN")
                self.assertEqual(calls, ["prepare", "activate", "click", "result:unknown"])

    def test_preclick_lookup_failure_can_use_another_locator(self) -> None:
        calls: list[str] = []

        class Client:
            def prepare_action(self, *_args: object) -> dict[str, str]:
                return {"permitId": "permit-1"}

            def activate_action(self, *_args: object) -> dict[str, bool]:
                return {"activated": True}

            def action_result(self, _lease: object, _permit: str, outcome: str) -> dict[str, bool]:
                calls.append("result:" + outcome)
                return {"continue": True}

        class Locator:
            def __init__(self, missing: bool) -> None:
                self.missing = missing

            def count(self) -> int:
                if self.missing:
                    calls.append("lookup-unavailable")
                    raise RuntimeError("detached during lookup; no input dispatched")
                return 1

            def is_visible(self) -> bool:
                return True

            def is_enabled(self) -> bool:
                return True

            def click(self, timeout: int) -> None:
                calls.append("click")

        class Frame:
            url = "https://pay.openai.com/pay/test"

            def locator(self, selector: str) -> Locator:
                return Locator(missing=selector == "#payment-submit")

        class Page:
            frames = [Frame()]

        result = LiveExecutor()._activate(
            Client(), lease(), Page(), {"stateId": "PAYMENT_FINAL_READY"}, "submit", "payment-submit"
        )
        self.assertTrue(result["continue"])
        self.assertEqual(calls, ["lookup-unavailable", "click", "result:clicked"])

    def test_no_click_candidate_is_distinct_from_unknown_click_outcome(self) -> None:
        client = mock.Mock()
        client.prepare_action.return_value = {"permitId": "permit-1"}
        page = mock.Mock(frames=[])
        command = lease()
        with self.assertRaises(ExecutorAPIError) as raised:
            LiveExecutor()._activate(
                client, command, page, {"stateId": "PAYMENT_FINAL_READY"}, "submit", "payment-submit"
            )
        self.assertEqual(raised.exception.code, "PERMIT_ACTIVATION_UNCERTAIN")
        client.action_result.assert_called_once_with(command, "permit-1", "not-clicked")

    def test_checkout_reports_only_submission_after_submit_activation(self) -> None:
        client = mock.Mock()
        executor = LiveExecutor()
        command = lease()
        facts = {
            "stateId": "PAYMENT_FINAL_READY", "structuralHash": "page-snapshot",
            "controls": {"submit": "payment-submit"},
        }
        page = object()
        with (
            mock.patch("python_executor.live._fill_frames"),
            mock.patch.object(executor, "_wait_facts", return_value=facts),
            mock.patch.object(executor, "_activate", return_value={"continue": False}) as activate,
        ):
            executor._checkout(client, command, {}, page, 0, initial=facts)
        activate.assert_called_once_with(client, command, page, facts, "submit", "payment-submit")
        client.report_submitted.assert_called_once_with(command, facts)
        client.report.assert_not_called()


if __name__ == "__main__":
    unittest.main()
