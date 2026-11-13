"""Run production checkout scripts against offline Node fetch/DOM doubles.

All responses, identities and page attributes are synthetic. These tests check
the scripts' guards and parsing; they do not establish current ChatGPT selectors,
Go purchase eligibility, a real Go SKU, or successful remote authentication.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import unittest
from pathlib import Path

from python_executor.live import PREPARE_PLUS_JS


# The VM receives only synthetic fetch and DOM implementations. It has neither
# Node's require/process nor the host's fetch, and no request is sent to a server.
NODE_RUNNER = r"""
const fs = require('node:fs');
const vm = require('node:vm');
const {source, fixture} = JSON.parse(fs.readFileSync(0, 'utf8'));
const calls = {sessionReads: 0, subscriptionReads: 0, checkoutPosts: 0, unexpected: 0};
const checkoutPlans = [];
const now = Date.parse('2026-11-13T12:00:00Z');
class FixtureDate extends Date { static now() { return now; } }
const response = data => ({ok: true, status: 200, json: async () => data});
const fetchDouble = async (url, options = {}) => {
  const method = options.method || 'GET';
  if (fixture.kind === 'entry' && url === '/api/auth/session' && method === 'GET') {
    calls.sessionReads++;
    return response({accessToken: 'synthetic.fixture.token', account: {id: 'synthetic-account'}});
  }
  if (fixture.kind === 'entry' && url === '/backend-api/subscriptions?account_id=synthetic-account' && method === 'GET') {
    calls.subscriptionReads++;
    return response({plan_type: fixture.subscriptionPlan, active_until: '2026-11-14T12:00:00Z', is_delinquent: false});
  }
  if (fixture.kind === 'entry' && url === '/backend-api/payments/checkout' && method === 'POST') {
    calls.checkoutPosts++;
    const payload = JSON.parse(options.body);
    checkoutPlans.push(payload.plan_name);
    return response({tag: 'hosted_checkout_session', url: 'https://pay.openai.com/pay/cs_synthetic'});
  }
  calls.unexpected++;
  throw new Error('unexpected synthetic request');
};
const elements = new Map((fixture.elements || []).map(item => [item.selector, {
  getAttribute: name => Object.prototype.hasOwnProperty.call(item.attributes, name) ? item.attributes[name] : null,
  textContent: item.text || '',
}]));
const context = vm.createContext({
  location: {
    origin: 'https://chatgpt.com', pathname: '/checkout',
    search: '?plan=go', href: 'https://chatgpt.com/checkout?plan=go',
  },
  document: {
    querySelector: selector => elements.get(selector) || null,
    body: {textContent: 'ChatGPT Go. Try Go. Go back.'},
    title: 'ChatGPT Go',
  },
  Date: FixtureDate,
  atob: value => Buffer.from(value, 'base64').toString('binary'),
  AbortController, setTimeout, clearTimeout,
  fetch: fetchDouble,
});
(async () => {
  const evaluated = vm.runInContext('(' + source + ')', context, {timeout: 1000});
  const result = await (typeof evaluated === 'function' ? evaluated() : evaluated);
  // Return only the fields under test; no identity, token or checkout URL.
  const facts = fixture.kind === 'entry'
    ? {errorKind: result.errorKind, responseTag: result.responseTag}
    : {plan: result.plan, country: result.country, currency: result.currency, displayedAmount: result.displayedAmount};
  process.stdout.write(JSON.stringify({facts, calls, checkoutPlans}));
})().catch(() => { process.stderr.write('offline plan script evaluation failed\n'); process.exitCode = 1; });
"""


@unittest.skipUnless(shutil.which("node"), "Node is unavailable for offline script doubles")
class PlanScriptsTest(unittest.TestCase):
    def go_source(self, constant: str) -> str:
        scripts = Path(__file__).resolve().parents[2] / "internal" / "checkout" / "scripts.go"
        source = re.search(
            rf"^const {re.escape(constant)} = `(.+?)`",
            scripts.read_text(encoding="utf-8"), re.MULTILINE | re.DOTALL,
        )
        self.assertIsNotNone(source, "production script constant must remain independently testable")
        return source.group(1)

    def run_source(self, source: str, fixture: dict[str, object]) -> dict[str, object]:
        process = subprocess.run(
            [shutil.which("node"), "-e", NODE_RUNNER],
            input=json.dumps({"source": source, "fixture": fixture}),
            text=True, capture_output=True, timeout=5, check=False,
        )
        self.assertEqual(process.returncode, 0, "offline plan script evaluation failed")
        output = json.loads(process.stdout)
        self.assertEqual(output["calls"]["unexpected"], 0, "script attempted an unexpected request")
        return output

    def inspect_plan(self, plan_element: dict[str, object] | None) -> dict[str, object]:
        # These attributes exercise the explicit parser contract, not a claimed
        # snapshot of today's production checkout page.
        elements = [
            {"selector": 'meta[name="openai-country"]', "attributes": {"content": "PH"}},
            {"selector": 'meta[name="openai-currency"]', "attributes": {"content": "PHP"}},
            {"selector": 'meta[name="openai-amount"]', "attributes": {"content": "123.45"}},
        ]
        if plan_element is not None:
            elements.append(plan_element)
        output = self.run_source(self.go_source("inspectFrameJS"), {"kind": "inspect", "elements": elements})
        self.assertEqual(output["calls"], {"sessionReads": 0, "subscriptionReads": 0, "checkoutPosts": 0, "unexpected": 0})
        self.assertEqual(output["facts"]["country"], "PH")
        self.assertEqual(output["facts"]["currency"], "PHP")
        self.assertEqual(output["facts"]["displayedAmount"], 123.45)
        return output["facts"]

    def test_active_go_subscription_stops_before_checkout_post(self) -> None:
        for adapter, source in (("go", self.go_source("preparePlusCheckoutJS")), ("python", PREPARE_PLUS_JS)):
            for plan in ("go", "Go", "GO"):
                with self.subTest(adapter=adapter, subscription_plan=plan):
                    output = self.run_source(source, {"kind": "entry", "subscriptionPlan": plan})
                    self.assertEqual(output["facts"]["errorKind"], "already_subscribed")
                    self.assertEqual(output["calls"], {"sessionReads": 1, "subscriptionReads": 1, "checkoutPosts": 0, "unexpected": 0})
                    self.assertEqual(output["checkoutPlans"], [])

    def test_synthetic_free_control_still_requests_only_the_existing_plus_sku(self) -> None:
        # A positive control proves the fetch double would observe a POST. It
        # returns an in-memory response and never creates a real checkout.
        for adapter, source in (("go", self.go_source("preparePlusCheckoutJS")), ("python", PREPARE_PLUS_JS)):
            with self.subTest(adapter=adapter):
                output = self.run_source(source, {"kind": "entry", "subscriptionPlan": "free"})
                self.assertEqual(output["facts"], {"errorKind": "", "responseTag": "hosted_checkout_session"})
                self.assertEqual(output["calls"], {"sessionReads": 1, "subscriptionReads": 1, "checkoutPosts": 1, "unexpected": 0})
                self.assertEqual(output["checkoutPlans"], ["chatgptplusplan"])

    def test_explicit_go_attribute_is_recognized_by_each_existing_source(self) -> None:
        for selector, attribute in (
            ("[data-kw-plan]", "data-kw-plan"),
            ('[data-testid="plan-name"][data-plan]', "data-plan"),
            ('meta[name="openai-plan"]', "content"),
        ):
            with self.subTest(source=selector):
                facts = self.inspect_plan({"selector": selector, "attributes": {attribute: "go"}})
                self.assertEqual(facts["plan"], "go")

    def test_missing_plan_is_not_inferred_from_page_title_text_or_url(self) -> None:
        self.assertIsNone(self.inspect_plan(None)["plan"])

    def test_unknown_or_empty_explicit_plan_is_not_guessed(self) -> None:
        for value in ("synthetic-unrecognized-plan", "Go back", "", "   "):
            with self.subTest(attribute_value=value):
                facts = self.inspect_plan({"selector": "[data-kw-plan]", "attributes": {"data-kw-plan": value}})
                self.assertIsNone(facts["plan"])

    def test_explicit_plus_is_not_relabelled_by_visible_go_text(self) -> None:
        facts = self.inspect_plan({"selector": "[data-kw-plan]", "attributes": {"data-kw-plan": "plus"}})
        self.assertEqual(facts["plan"], "plus")


if __name__ == "__main__":
    unittest.main()
