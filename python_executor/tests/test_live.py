from __future__ import annotations

import unittest

from python_executor.client import ExecutorAPIError, ExecutorLease
from python_executor.live import (
    LiveExecutor,
    _classify,
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


if __name__ == "__main__":
    unittest.main()
