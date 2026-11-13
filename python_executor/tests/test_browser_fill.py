"""Real Chromium DOM tests using only local, synthetic routed HTML.

Run from the repository root:
    python3 -m unittest discover -s python_executor/tests -p test_browser_fill.py -v

Playwright's installed headless shell is used by default. To test an existing
full Chromium binary, set KWMEMBERSHIP_BROWSER_TEST_CHROME_PATH to its path.
Known installation/IPC restrictions are reported as explicit skipped tests;
DOM/assertion failures are never converted into skips. These tests do not use
the production account/browser profile or establish live ChatGPT compatibility.
Set KWMEMBERSHIP_REQUIRE_BROWSER_TESTS=1 in CI to fail on unavailable browsers.
"""

from __future__ import annotations

import copy
import os
import re
import unittest
from contextlib import suppress
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import urlsplit

from python_executor.client import ExecutorAPIError
from python_executor.live import FILL_FRAME_JS, _fill_frames


# Synthetic test card and invented fixture address. Never a real billing profile.
SYNTHETIC_MATERIAL = {
    "card": {"Number": "4242424242424242", "ExpiryMonth": "1", "ExpiryYear": "2030", "CVV": "123"},
    "billing": {
        "Name": "Synthetic Fixture", "Line1": "1 Test Street", "City": "Test City",
        "State": "Test State", "Country": "PH", "PostalCode": "00000",
    },
}
SYNTHETIC_FRAGMENT = {
    "cardNumber": "4242424242424242", "expiry": "1/30", "expiryMonth": "1", "expiryYear": "2030", "cvc": "123",
    "billingName": "Synthetic Fixture", "billingLine1": "1 Test Street", "billingLine2": "", "billingCity": "Test City",
    "billingState": "Test State", "billingCountry": "PH", "billingPostal": "00000",
}
FIXTURE_ORIGINS = {"https://pay.openai.com", "https://js.stripe.com", "https://untrusted.example"}
FIXTURE_PATHS = {"/pay/offline-fixture", "/v3/offline-fixture"}


def browser_unavailability(error: Exception) -> str | None:
    """Recognize setup restrictions without emitting browser logs or arguments."""
    message = str(error)
    if "process_singleton_posix.cc" in message and "socket() failed: Operation not permitted" in message:
        return "CHROMIUM_UNIX_SOCKET_DENIED: full Chromium cannot create its ProcessSingleton AF_UNIX socket in this execution environment"
    if "Executable doesn't exist" in message:
        return "CHROMIUM_NOT_INSTALLED: install the matching Playwright headless shell or configure KWMEMBERSHIP_BROWSER_TEST_CHROME_PATH"
    if "error while loading shared libraries" in message or "Host system is missing dependencies" in message:
        return "CHROMIUM_SYSTEM_DEPENDENCIES_MISSING: provision official browser runtime dependencies in the approved environment"
    return None


class BrowserFillTest(unittest.TestCase):
    @classmethod
    def mark_unavailable(cls, reason: str) -> None:
        if os.environ.get("KWMEMBERSHIP_REQUIRE_BROWSER_TESTS") == "1":
            raise RuntimeError(reason + "; browser tests are required and cannot be skipped") from None
        cls.unavailable = reason

    @classmethod
    def setUpClass(cls) -> None:
        cls.browser = None
        cls.playwright = None
        cls.unavailable = None
        try:
            from playwright.sync_api import sync_playwright
        except ImportError:
            cls.mark_unavailable("PLAYWRIGHT_NOT_INSTALLED: install python_executor/requirements.txt in the test environment")
            return
        cls.playwright = sync_playwright().start()
        options = {
            "headless": True,
            "timeout": 15000,
            # Every fixture request is fulfilled locally below. This closed
            # loopback proxy also prevents accidental outbound browser traffic.
            "proxy": {"server": "http://127.0.0.1:9"},
            "args": ["--disable-background-networking"],
        }
        executable = os.environ.get("KWMEMBERSHIP_BROWSER_TEST_CHROME_PATH", "").strip()
        if executable:
            options["executable_path"] = executable
        try:
            cls.browser = cls.playwright.chromium.launch(**options)
        except Exception as error:
            reason = browser_unavailability(error)
            with suppress(Exception):
                cls.playwright.stop()
            cls.playwright = None
            if reason is None:
                raise
            cls.mark_unavailable(reason)

    @classmethod
    def tearDownClass(cls) -> None:
        if cls.browser is not None:
            cls.browser.close()
        if cls.playwright is not None:
            cls.playwright.stop()

    def setUp(self) -> None:
        if self.unavailable:
            self.skipTest(self.unavailable)
        self.context = self.browser.new_context(service_workers="block")
        self.addCleanup(self.context.close)
        self.context.set_default_timeout(5000)
        fixture = Path(__file__).with_name("browser_fill.html").read_text(encoding="utf-8")
        self.blocked_requests = 0
        self.expected_blocked_requests = 0
        self.unmarked_responses = 0
        fixture_headers = {"X-KW-Offline-Fixture": "1"}

        def route_locally(route) -> None:
            parsed = urlsplit(route.request.url)
            known_origin = f"{parsed.scheme}://{parsed.netloc}" in FIXTURE_ORIGINS
            if known_origin and parsed.path in FIXTURE_PATHS:
                route.fulfill(status=200, content_type="text/html", headers=fixture_headers, body=fixture)
            elif known_origin and parsed.path == "/favicon.ico":
                route.fulfill(status=204, headers=fixture_headers, body="")
            else:
                self.blocked_requests += 1
                route.abort()

        def check_local_response(response) -> None:
            if response.headers.get("x-kw-offline-fixture") != "1":
                self.unmarked_responses += 1

        # No route is continued to a real host; no local HTTP server is needed.
        self.context.route("**/*", route_locally)
        self.context.on("response", check_local_response)
        self.page = self.context.new_page()

    def tearDown(self) -> None:
        if not self.unavailable:
            self.assertEqual(self.unmarked_responses, 0, "a response did not come from the local fixture router")
            self.assertEqual(self.blocked_requests, self.expected_blocked_requests, "the fixture attempted an unexpected network request")

    def open_fixture(self, scenario: str, origin: str = "https://pay.openai.com") -> None:
        self.page.goto(origin + "/pay/offline-fixture?scenario=" + scenario, wait_until="load")
        self.page.wait_for_function("window.syntheticFixture?.ready === true")

    def assert_fill_rejected(self, scenario: str) -> None:
        self.open_fixture(scenario)
        with self.assertRaises(ExecutorAPIError) as raised:
            _fill_frames(self.page, copy.deepcopy(SYNTHETIC_MATERIAL))
        self.assertEqual(raised.exception.code, "PAYMENT_FIELDS_NOT_FILLED")
        self.assertFalse(self.page.evaluate("window.syntheticFixture.submitted"))

    def test_formats_number_and_expiry_in_real_dom(self) -> None:
        self.open_fixture("formatted")
        result = self.page.evaluate(FILL_FRAME_JS, copy.deepcopy(SYNTHETIC_FRAGMENT))
        self.assertEqual(set(result), {"accepted", "filled", "rejected"})
        self.assertTrue(result["accepted"])
        self.assertEqual(result["rejected"], [])
        self.assertIn("cardNumber", result["filled"])
        self.assertIn("expiry", result["filled"])
        # Only booleans leave the browser; assertions do not print card values.
        self.assertTrue(self.page.evaluate("document.querySelector('#payment-numberInput').value.includes(' ')"))
        self.assertTrue(self.page.evaluate("document.querySelector('#payment-expiryInput').value === '01 / 2030'"))

    def test_normalizes_native_month_and_year_selects(self) -> None:
        self.open_fixture("normalized-selects")
        _fill_frames(self.page, copy.deepcopy(SYNTHETIC_MATERIAL))
        self.assertTrue(self.page.evaluate("document.querySelector('#payment-expiryMonthInput').value === '01'"))
        self.assertTrue(self.page.evaluate("document.querySelector('#payment-expiryYearInput').value === '30'"))

    def test_nonempty_line2_must_be_accepted_by_a_real_field(self) -> None:
        material = copy.deepcopy(SYNTHETIC_MATERIAL)
        material["billing"]["Line2"] = "Synthetic suite 2"
        self.open_fixture("line2")
        _fill_frames(self.page, material)
        self.assertTrue(self.page.evaluate("document.querySelector('#billingAddress-line2Input').value === 'Synthetic suite 2'"))
        for scenario in ("line2-truncated", "formatted"):
            with self.subTest(scenario=scenario):
                self.open_fixture(scenario)
                with self.assertRaises(ExecutorAPIError) as raised:
                    _fill_frames(self.page, material)
                self.assertEqual(raised.exception.code, "PAYMENT_FIELDS_NOT_FILLED")

    def test_go_fill_script_also_reads_back_line2_in_real_dom(self) -> None:
        scripts = Path(__file__).resolve().parents[2] / "internal" / "checkout" / "scripts.go"
        source = re.search(r"const fillFrameFunction = `(.+?)`", scripts.read_text(), re.DOTALL)
        self.assertIsNotNone(source)
        fragment = {**SYNTHETIC_FRAGMENT, "billingLine2": "Synthetic suite 2"}
        for scenario, result_key in (("line2", "filled"), ("line2-truncated", "rejected")):
            with self.subTest(scenario=scenario):
                self.open_fixture(scenario)
                result = self.page.evaluate(source.group(1), fragment)
                self.assertIn("billingLine2", result[result_key])

    def test_empty_state_and_postal_follow_real_required_attributes(self) -> None:
        material = copy.deepcopy(SYNTHETIC_MATERIAL)
        material["billing"]["State"] = ""
        material["billing"]["PostalCode"] = ""
        self.open_fixture("optional-state-postal")
        _fill_frames(self.page, material)
        self.assertTrue(self.page.evaluate("document.querySelector('#billingAddress-stateInput').value === '' && document.querySelector('#billingAddress-postalCodeInput').value === ''"))
        self.open_fixture("no-state-postal")
        _fill_frames(self.page, material)
        for scenario in ("formatted", "required-state", "aria-required-state"):
            with self.subTest(scenario=scenario):
                self.open_fixture(scenario)
                evidence = copy.deepcopy(material)
                if scenario != "formatted":
                    evidence["billing"]["PostalCode"] = "00000"
                with self.assertRaises(ExecutorAPIError) as raised:
                    _fill_frames(self.page, evidence)
                self.assertEqual(raised.exception.code, "PAYMENT_FIELDS_NOT_FILLED")

    def test_rejects_input_reset_by_its_event_handler(self) -> None:
        self.assert_fill_rejected("sync-reset")

    def test_rechecks_node_replaced_during_real_animation_frame(self) -> None:
        self.assert_fill_rejected("render-replacement")
        self.assertTrue(self.page.evaluate("window.syntheticFixture.replaced"))

    def test_rejects_native_select_validity_and_optional_invalid_fields(self) -> None:
        for scenario in ("unsupported-select", "native-invalid", "optional-invalid"):
            with self.subTest(scenario=scenario):
                self.assert_fill_rejected(scenario)

    def test_distributes_fields_only_to_allowed_iframes(self) -> None:
        self.open_fixture("frames")
        records = []

        class ObservedFrame:
            def __init__(self, frame):
                self.frame = frame

            @property
            def url(self):
                return self.frame.url

            def evaluate(self, source, argument):
                # Record origin and argument presence, never material values.
                parsed = urlsplit(self.url)
                records.append((parsed.hostname, argument is not None))
                return self.frame.evaluate(source, argument)

        observed = SimpleNamespace(frames=[ObservedFrame(frame) for frame in self.page.frames])
        _fill_frames(observed, copy.deepcopy(SYNTHETIC_MATERIAL))
        self.assertTrue(any(host == "js.stripe.com" and supplied for host, supplied in records))
        self.assertFalse(any(host == "untrusted.example" for host, _ in records))
        foreign = self.page.frame(name="synthetic-foreign")
        self.assertIsNotNone(foreign)
        self.assertTrue(foreign.evaluate("window.syntheticFixture.writes === 0 && [...document.querySelectorAll('input')].every(element => element.value === '')"))

    def test_javascript_guard_refuses_foreign_document_without_writing(self) -> None:
        self.open_fixture("foreign-form", "https://untrusted.example")
        result = self.page.evaluate(FILL_FRAME_JS, copy.deepcopy(SYNTHETIC_FRAGMENT))
        self.assertEqual(result, {"accepted": False, "filled": [], "rejected": []})
        self.assertTrue(self.page.evaluate("window.syntheticFixture.writes === 0"))

    def test_invalid_optional_iframe_cannot_be_hidden_by_complete_top_form(self) -> None:
        self.assert_fill_rejected("full-with-invalid-frame")

    def test_empty_hidden_allowed_iframe_does_not_require_animation_frames(self) -> None:
        self.open_fixture("full-with-empty-frame")
        _fill_frames(self.page, copy.deepcopy(SYNTHETIC_MATERIAL))
        empty = self.page.frame(name="synthetic-empty")
        self.assertIsNotNone(empty)
        self.assertTrue(empty.evaluate("window.syntheticFixture.writes === 0 && window.syntheticFixture.rafRequested === false"))

    def test_unregistered_http_request_is_aborted_without_a_response(self) -> None:
        self.open_fixture("formatted")
        self.expected_blocked_requests = 1
        blocked = self.page.evaluate("""async () => {
          try { await fetch('https://offline-network-denied.invalid/no-network', {mode: 'no-cors'}); return false; }
          catch { return true; }
        }""")
        self.assertTrue(blocked)
        self.assertEqual(self.blocked_requests, 1)
        self.assertEqual(self.unmarked_responses, 0)


if __name__ == "__main__":
    unittest.main()
