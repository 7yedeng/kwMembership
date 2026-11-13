"""Three independent Chromium lifecycles, using only .invalid local fixtures.

This test patches only the origin constant in the in-memory fill script and
Python test process. It does NOT validate or modify the production allowlist.
All requests must be locally fulfilled or aborted, and a closed loopback proxy
also prevents accidental browser egress. No provider SDK or real credential.
"""
from __future__ import annotations

import copy
import os
from pathlib import Path
import time
import unittest
from unittest import mock
from urllib.parse import urlsplit

from python_executor.client import ExecutorAPIError, ExecutorLease
from python_executor import live
from python_executor.tests.test_browser_fill import browser_unavailability


EXPECTED_LIFECYCLES = 3
MAIN = "https://checkout.fixture.invalid"
CARD = "https://card.fixture.invalid"
FOREIGN = "https://foreign.fixture.invalid"
ALL_FIXTURE_ORIGINS = frozenset({MAIN, CARD, FOREIGN})
FILL_FIXTURE_ORIGINS = frozenset({MAIN, CARD})
ORIGINAL_GUARD = "const allowed = ['https://chatgpt.com', 'https://pay.openai.com', 'https://js.stripe.com'];"
FIXTURE_GUARD = "const allowed = ['https://checkout.fixture.invalid', 'https://card.fixture.invalid'];"
RESTART_EVIDENCE: list[dict[str, object]] = []


def fixture_fill_script() -> str:
    if live.FILL_FRAME_JS.count(ORIGINAL_GUARD) != 1:
        raise AssertionError("fill origin declaration changed; review fixture injection")
    return live.FILL_FRAME_JS.replace(ORIGINAL_GUARD, FIXTURE_GUARD, 1)


def synthetic_material(index: int) -> dict[str, dict[str, str]]:
    # Published test number, invented address, no valid payment destination.
    return {
        "card": {"Number": "4242424242424242", "ExpiryMonth": str(index), "ExpiryYear": str(2029 + index), "CVV": str(120 + index)},
        "billing": {"Name": f"Synthetic Restart {index}", "Line1": f"{index} Fixture Street",
                    "Line2": f"Unit {index}", "City": f"Fixture City {index}", "State": f"Fixture State {index}",
                    "Country": ("PH", "EG", "US")[index - 1], "PostalCode": f"0000{index}"},
    }


class LocalFixtureRouter:
    def __init__(self, fixture: str):
        self.fixture = fixture
        self.fulfilled = 0
        self.aborted = 0
        self.unmarked_responses = 0
        self.websockets = 0

    def route(self, route) -> None:
        parsed = urlsplit(route.request.url)
        known = f"{parsed.scheme}://{parsed.netloc}" in ALL_FIXTURE_ORIGINS
        if known and route.request.method == "GET" and parsed.path in {"/fixture", "/favicon.ico"}:
            self.fulfilled += 1
            headers = {
                "X-KW-Offline-Fixture": "restart-v1",
                "Cache-Control": "no-store",
                "Content-Security-Policy": "default-src 'none'; script-src 'unsafe-inline'; frame-src https://card.fixture.invalid https://foreign.fixture.invalid; form-action 'none'; connect-src 'none'",
            }
            route.fulfill(status=204 if parsed.path == "/favicon.ico" else 200,
                          content_type="text/html", headers=headers,
                          body="" if parsed.path == "/favicon.ico" else self.fixture)
        else:
            self.aborted += 1
            route.abort()

    def response(self, response) -> None:
        if response.headers.get("x-kw-offline-fixture") != "restart-v1":
            self.unmarked_responses += 1

    def websocket(self, route) -> None:
        self.websockets += 1
        route.close()


class StopBeforeSubmit:
    def __init__(self):
        self.prepared = 0
        self.activated = 0
        self.reported_submitted = 0

    def prepare_action(self, _lease, kind, control_id, _facts):
        if kind != "submit" or control_id != "payment-submit":
            raise AssertionError("unexpected action reached the fixture guard")
        self.prepared += 1
        raise ExecutorAPIError("FIXTURE_STOP_BEFORE_SUBMIT", 409)

    def activate_action(self, *_args):
        self.activated += 1
        raise AssertionError("the fixture must never grant an action permit")

    def report_submitted(self, *_args):
        self.reported_submitted += 1
        raise AssertionError("the fixture must never report submission")


def synthetic_lease() -> ExecutorLease:
    return ExecutorLease(execution_id="offline-restart", executor_id="offline-fixture", lease_epoch=1,
                         lease_token="synthetic-not-a-credential", hard_deadline_at="2099-01-01T00:00:00Z",
                         command_kind="payment", stage="plus", attempt_no=1, target_tier="plus",
                         adapter_version="fixture-restart-only", price_contract={})


class RestartContractTest(unittest.TestCase):
    """Offline checks still run when this host cannot launch Chromium."""

    def test_origin_injection_changes_only_the_test_guard(self):
        test_script = fixture_fill_script()
        self.assertEqual(test_script.replace(FIXTURE_GUARD, ORIGINAL_GUARD, 1), live.FILL_FRAME_JS)
        self.assertNotEqual(FILL_FIXTURE_ORIGINS, live.FILL_ORIGINS)
        self.assertFalse(live._allowed_fill_origin(MAIN + "/fixture"))

    def test_router_never_continues_a_real_or_unregistered_request(self):
        for value, method in (("https://outside.example/request", "GET"),
                              (MAIN + "/fixture-payment", "POST"),
                              (MAIN + "/unexpected", "GET"),
                              ("https://checkout.fixture.invalid.evil.example/fixture", "GET")):
            with self.subTest(method=method):
                router = LocalFixtureRouter("<p>synthetic</p>")
                request = mock.Mock(url=value, method=method)
                route = mock.Mock(request=request)
                router.route(route)
                route.abort.assert_called_once_with()
                route.fulfill.assert_not_called()
                route.continue_.assert_not_called()
                self.assertEqual(router.aborted, 1)

    def test_router_marks_every_local_response_and_closes_websockets(self):
        router = LocalFixtureRouter("<p>synthetic</p>")
        route = mock.Mock(request=mock.Mock(url=MAIN + "/fixture", method="GET"))
        router.route(route)
        self.assertEqual(route.fulfill.call_args.kwargs["headers"]["X-KW-Offline-Fixture"], "restart-v1")
        route.continue_.assert_not_called()
        socket = mock.Mock()
        router.websocket(socket)
        socket.close.assert_called_once_with()

    def test_production_checkout_stops_before_click_when_permit_is_refused(self):
        guard = StopBeforeSubmit()
        executor = live.LiveExecutor()
        facts = {"stateId": "PAYMENT_FINAL_READY", "structuralHash": "offline-contract",
                 "controls": {"submit": "payment-submit"}}
        with mock.patch.object(live, "_fill_frames") as fill, \
             mock.patch.object(executor, "_wait_facts", return_value=facts), \
             mock.patch.object(live, "_activate_frames") as click:
            with self.assertRaises(ExecutorAPIError) as raised:
                executor._checkout(guard, synthetic_lease(), {}, object(), time.time() + 1, initial=facts)
            fill.assert_called_once()
            click.assert_not_called()
        self.assertEqual(raised.exception.code, "FIXTURE_STOP_BEFORE_SUBMIT")
        self.assertEqual((guard.prepared, guard.activated, guard.reported_submitted), (1, 0, 0))


class BrowserRestartTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.playwright = None
        cls.unavailable = None
        try:
            from playwright.sync_api import sync_playwright
        except ImportError:
            cls.mark_unavailable("PLAYWRIGHT_NOT_INSTALLED")
            return
        cls.playwright = sync_playwright().start()

    @classmethod
    def mark_unavailable(cls, reason: str):
        if os.environ.get("KWMEMBERSHIP_REQUIRE_BROWSER_TESTS") == "1":
            raise RuntimeError(reason + "; required restart acceptance cannot be skipped") from None
        cls.unavailable = reason

    @classmethod
    def tearDownClass(cls):
        if cls.playwright is not None:
            cls.playwright.stop()

    def launch(self):
        options = {"headless": True, "timeout": 15000, "proxy": {"server": "http://127.0.0.1:9"},
                   "args": ["--disable-background-networking"]}
        executable = os.environ.get("KWMEMBERSHIP_BROWSER_TEST_CHROME_PATH", "").strip()
        if executable:
            options["executable_path"] = executable
        try:
            return self.playwright.chromium.launch(**options)
        except Exception as error:
            reason = browser_unavailability(error)
            if reason is None:
                raise RuntimeError("CHROMIUM_RESTART_UNEXPECTED_START_FAILURE") from None
            if RESTART_EVIDENCE or os.environ.get("KWMEMBERSHIP_REQUIRE_BROWSER_TESTS") == "1":
                raise RuntimeError(reason + "; restart sequence did not complete") from None
            self.skipTest(reason)

    def test_three_fresh_browser_lifecycles_stop_before_submission(self):
        RESTART_EVIDENCE.clear()
        if self.unavailable:
            self.skipTest(self.unavailable)
        fixture = Path(__file__).with_name("browser_restart.html").read_text(encoding="utf-8")
        prior_browser = None
        for index, variant in enumerate(("formatted", "split", "plain"), start=1):
            # Do not put startup inside subTest: a known setup skip would then
            # be swallowed and unnecessarily retry the same environment.
            if prior_browser is not None:
                self.assertFalse(prior_browser.is_connected(), "previous browser remained connected")
            browser = self.launch()
            prior_browser = browser
            disconnected = []
            browser.on("disconnected", lambda: disconnected.append(True))
            try:
                evidence = self.run_lifecycle(browser, fixture, index, variant)
            finally:
                browser.close()
            self.assertFalse(browser.is_connected(), "browser did not close")
            self.assertEqual(len(disconnected), 1, "owned lifecycle did not disconnect exactly once")
            evidence["browserClosed"] = True
            RESTART_EVIDENCE.append(evidence)
        self.assertEqual(len(RESTART_EVIDENCE), EXPECTED_LIFECYCLES, "not all fresh browser lifecycles passed")

    def run_lifecycle(self, browser, fixture, index, variant):
        context = browser.new_context(service_workers="block", accept_downloads=False)
        context.set_default_timeout(5000)
        # Before any page, the fresh browser/context must have no old state.
        self.assertEqual(context.cookies(), [], "fresh browser inherited cookies")
        self.assertEqual(context.storage_state()["origins"], [], "fresh browser inherited local storage")
        router = LocalFixtureRouter(fixture)
        context.route("**/*", router.route)
        context.route_web_socket("**/*", router.websocket)
        context.on("response", router.response)
        page = context.new_page()
        page.goto(MAIN + "/fixture?mode=main&variant=" + variant, wait_until="load")
        frames = [page.main_frame, page.frame(name="synthetic-card"), page.frame(name="synthetic-foreign")]
        self.assertTrue(all(frames), "fixture iframes did not load")
        for frame in frames:
            frame.wait_for_function("window.restartFixture?.ready === true")
            self.assertTrue(frame.evaluate("""() =>
                document.cookie === '' && localStorage.length === 0 && sessionStorage.length === 0 &&
                window.previousTask === undefined && document.querySelector('#fixture').dataset.task === undefined &&
                [...document.querySelectorAll('input,select')].every(element => element.value === '') &&
                restartFixture.writes === 0 && restartFixture.clicks === 0 && restartFixture.submissions === 0
            """), "a new lifecycle inherited browser or document state")

        material = synthetic_material(index)
        facts = {"stateId": "PAYMENT_FINAL_READY", "structuralHash": "fixture-final-review",
                 "controls": {"submit": "payment-submit"}}
        guard = StopBeforeSubmit()
        executor = live.LiveExecutor()
        # Only the server-validated page state is a stub. Actual DOM filling and
        # the production _checkout -> _activate -> prepare_action order execute.
        with mock.patch.object(live, "FILL_ORIGINS", FILL_FIXTURE_ORIGINS), \
             mock.patch.object(live, "FILL_FRAME_JS", fixture_fill_script()), \
             mock.patch.object(executor, "_wait_facts", return_value=copy.deepcopy(facts)):
            with self.assertRaises(ExecutorAPIError) as stopped:
                executor._checkout(guard, synthetic_lease(), copy.deepcopy(material), page,
                                   time.time() + 10, initial=copy.deepcopy(facts))
        self.assertEqual(stopped.exception.code, "FIXTURE_STOP_BEFORE_SUBMIT")
        self.assertEqual((guard.prepared, guard.activated, guard.reported_submitted), (1, 0, 0))

        billing = material["billing"]
        expected_billing = {
            "billingAddress-nameInput": billing["Name"], "billingAddress-line1Input": billing["Line1"],
            "billingAddress-line2Input": billing["Line2"], "billingAddress-cityInput": billing["City"],
            "billingAddress-stateInput": billing["State"], "billingAddress-countryInput": billing["Country"],
            "billingAddress-postalCodeInput": billing["PostalCode"],
        }
        exact = "expected => Object.entries(expected).every(([id,value]) => document.getElementById(id)?.value === value)"
        self.assertTrue(frames[0].evaluate(exact, expected_billing), "billing values did not exactly match this lifecycle")
        card = material["card"]
        expected_card = {"payment-numberInput": "4242 4242 4242 4242" if variant == "formatted" else card["Number"],
                         "payment-cvcInput": card["CVV"]}
        if variant == "split":
            expected_card.update({"payment-expiryMonthInput": card["ExpiryMonth"].zfill(2), "payment-expiryYearInput": card["ExpiryYear"][-2:]})
        else:
            expected_card["payment-expiryInput"] = (card["ExpiryMonth"].zfill(2) + " / " + card["ExpiryYear"] if variant == "formatted"
                                                     else card["ExpiryMonth"] + "/" + card["ExpiryYear"][-2:])
        self.assertTrue(frames[1].evaluate(exact, expected_card), "card iframe values did not exactly match this lifecycle")
        self.assertTrue(frames[2].evaluate("restartFixture.writes === 0 && [...document.querySelectorAll('input,select')].every(element => element.value === '')"),
                        "an untrusted iframe received material")
        for frame in frames:
            self.assertTrue(frame.evaluate("restartFixture.clicks === 0 && restartFixture.submissions === 0"), "fixture was submitted or clicked")

        # A later controlled-input reset must reject the readback and stop even
        # before asking for a submit permit, in every fresh browser lifecycle.
        frames[1].evaluate("document.querySelector('#payment-cvcInput').addEventListener('input', event => { event.target.value = ''; })")
        rejected_guard = StopBeforeSubmit()
        with mock.patch.object(live, "FILL_ORIGINS", FILL_FIXTURE_ORIGINS), \
             mock.patch.object(live, "FILL_FRAME_JS", fixture_fill_script()):
            with self.assertRaises(ExecutorAPIError) as rejected:
                executor._checkout(rejected_guard, synthetic_lease(), copy.deepcopy(material), page,
                                   time.time() + 10, initial=copy.deepcopy(facts))
        self.assertEqual(rejected.exception.code, "PAYMENT_FIELDS_NOT_FILLED")
        self.assertEqual(rejected_guard.prepared, 0, "rejected field reached action authorization")
        for frame in frames:
            self.assertTrue(frame.evaluate("restartFixture.clicks === 0 && restartFixture.submissions === 0"))
            # Plant synthetic old-task state, then prove the next process starts
            # clean. No real account cookie or persisted output file is used.
            self.assertTrue(frame.evaluate("""marker => {
              document.cookie = 'kw_restart_marker=' + marker + ';path=/;Secure;SameSite=Lax';
              localStorage.setItem('kw_restart_marker', marker);
              sessionStorage.setItem('kw_restart_marker', marker);
              window.previousTask = marker;
              document.querySelector('#fixture').dataset.task = marker;
              return document.cookie.includes(marker) && localStorage.getItem('kw_restart_marker') === marker &&
                sessionStorage.getItem('kw_restart_marker') === marker && window.previousTask === marker;
            }""", f"synthetic-run-{index}"))
        self.assertGreaterEqual(len(context.cookies()), 3, "old-state markers were not actually written")
        self.assertGreaterEqual(router.fulfilled, 3)
        self.assertEqual((router.aborted, router.unmarked_responses, router.websockets), (0, 0, 0), "unexpected request or nonlocal response")
        context.close()
        return {"lifecycle": index, "variant": variant, "initialStateEmpty": True, "exactReadback": True,
                "iframeIsolation": True, "stoppedBeforeSubmit": True, "resetRejectedBeforePermit": True,
                "staleStateSeeded": True, "unmarkedResponses": 0, "unexpectedRequests": 0, "paymentSubmitted": False}


if __name__ == "__main__":
    unittest.main()
