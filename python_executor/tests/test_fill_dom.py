"""Execute the production JS against offline Node DOM doubles.

These tests cover value acceptance and render replacement logic. They do not
validate real browser/React/Stripe behavior or any current production selector.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import unittest
from pathlib import Path

from python_executor.live import FILL_FRAME_JS


@unittest.skipUnless(shutil.which("node"), "Node is unavailable for offline DOM doubles")
class FillDOMTest(unittest.TestCase):
    def run_fixture(self, scenario: str) -> dict[str, object]:
        scripts = Path(__file__).resolve().parents[2] / "internal" / "checkout" / "scripts.go"
        go_source = re.search(r"const fillFrameFunction = `(.+?)`", scripts.read_text(), re.DOTALL)
        self.assertIsNotNone(go_source, "Go fill function must remain independently testable")
        outputs = []
        for adapter, source in (("python", FILL_FRAME_JS), ("go", go_source.group(1))):
            with self.subTest(adapter=adapter, scenario=scenario):
                outputs.append(self.run_adapter_fixture(source, scenario))
        self.assertEqual(outputs[0], outputs[1], "Go and Python fill adapters disagree")
        return outputs[0]

    def run_adapter_fixture(self, source: str, scenario: str) -> dict[str, object]:
        process = subprocess.run(
            [shutil.which("node"), str(Path(__file__).with_name("fill_dom_double.cjs"))],
            input=json.dumps({"source": source, "scenario": scenario}),
            text=True, capture_output=True, timeout=5, check=False,
        )
        self.assertEqual(process.returncode, 0, "offline DOM evaluation failed")
        output = json.loads(process.stdout)
        result = output["result"]
        self.assertEqual(set(result), {"accepted", "filled", "rejected"})
        self.assertIsInstance(result["accepted"], bool)
        for key in ("filled", "rejected"):
            self.assertIsInstance(result[key], list)
            self.assertTrue(all(isinstance(field, str) for field in result[key]))
        if result["accepted"] and output["hasForm"]:
            self.assertTrue(output["crossedRender"], "verification must follow a render boundary")
        return output

    def test_formatted_number_and_expiry_are_accepted_after_readback(self) -> None:
        result = self.run_fixture("formatted")["result"]
        self.assertTrue(result["accepted"])
        self.assertEqual(result["rejected"], [])
        self.assertIn("cardNumber", result["filled"])
        self.assertIn("expiry", result["filled"])

    def test_existing_alternate_selector_is_used(self) -> None:
        result = self.run_fixture("fallback-selector")["result"]
        self.assertEqual(result["rejected"], [])
        self.assertIn("cardNumber", result["filled"])

    def test_month_and_year_select_representation_is_normalized(self) -> None:
        result = self.run_fixture("normalized-selects")["result"]
        self.assertEqual(result["rejected"], [])
        self.assertIn("expiryMonth", result["filled"])
        self.assertIn("expiryYear", result["filled"])

    def test_actual_billing_country_is_not_forced_to_price_contract_country(self) -> None:
        result = self.run_fixture("actual-billing-country")["result"]
        self.assertEqual(result["rejected"], [])
        self.assertIn("billingCountry", result["filled"])

    def test_select_missing_requested_option_is_rejected(self) -> None:
        result = self.run_fixture("unsupported-select")["result"]
        self.assertIn("billingCountry", result["rejected"])
        self.assertNotIn("billingCountry", result["filled"])

    def test_input_event_reset_is_rejected(self) -> None:
        result = self.run_fixture("sync-reset")["result"]
        self.assertIn("cvc", result["rejected"])
        self.assertNotIn("cvc", result["filled"])

    def test_element_replaced_on_render_is_queried_again(self) -> None:
        result = self.run_fixture("render-replacement")["result"]
        self.assertIn("cvc", result["rejected"])
        self.assertNotIn("cvc", result["filled"])

    def test_native_invalid_value_is_rejected(self) -> None:
        result = self.run_fixture("native-invalid")["result"]
        self.assertIn("billingPostal", result["rejected"])

    def test_optional_field_with_explicit_invalid_state_is_rejected(self) -> None:
        result = self.run_fixture("optional-invalid")["result"]
        self.assertIn("billingState", result["rejected"])

    def test_present_required_state_needs_supplied_material(self) -> None:
        result = self.run_fixture("optional-missing-material")["result"]
        self.assertIn("billingState", result["rejected"])

    def test_line2_requires_a_matching_readback(self) -> None:
        accepted = self.run_fixture("line2-accepted")["result"]
        self.assertIn("billingLine2", accepted["filled"])
        self.assertEqual(accepted["rejected"], [])
        rejected = self.run_fixture("line2-truncated")["result"]
        self.assertIn("billingLine2", rejected["rejected"])
        self.assertNotIn("billingLine2", rejected["filled"])

    def test_blank_optional_state_and_postal_are_not_fabricated_or_required(self) -> None:
        result = self.run_fixture("optional-empty-material")["result"]
        self.assertEqual(result["rejected"], [])
        self.assertNotIn("billingState", result["filled"])
        self.assertNotIn("billingPostal", result["filled"])

    def test_blank_state_and_postal_fail_when_the_page_requires_them(self) -> None:
        postal = self.run_fixture("required-empty-postal")["result"]
        self.assertIn("billingPostal", postal["rejected"])
        state = self.run_fixture("aria-required-empty-state")["result"]
        self.assertIn("billingState", state["rejected"])

    def test_new_required_field_after_render_cannot_escape_fragment_validation(self) -> None:
        result = self.run_fixture("new-required-after-render")["result"]
        self.assertIn("billingPostal", result["rejected"])

    def test_javascript_guard_refuses_foreign_origin_without_a_setter(self) -> None:
        output = self.run_fixture("foreign-origin")
        self.assertFalse(output["result"]["accepted"])
        self.assertFalse(output["wrote"])

    def test_empty_frame_does_not_require_animation_frames_or_claim_filled_fields(self) -> None:
        output = self.run_fixture("empty-non-rendering-frame")
        self.assertEqual(output["result"], {"accepted": True, "filled": [], "rejected": []})
        self.assertFalse(output["hasForm"])
        self.assertFalse(output["crossedRender"])
        self.assertFalse(output["wrote"])


if __name__ == "__main__":
    unittest.main()
