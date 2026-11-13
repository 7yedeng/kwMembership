package checkout

import (
	"testing"

	"kwmembership/internal/provider"
)

func syntheticBillingRequest() Request {
	return Request{
		Mode: ModeSessionCheckout, Stage: "plus", TargetTier: "plus", ExpectedEmail: "fixture@example.com",
		Cookies:       []provider.SessionCookie{{Name: "synthetic-cookie", Value: "synthetic-value"}},
		PriceContract: PriceContract{ID: "fixture-price", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1000, MaxAmount: 1200},
		Guard:         requestGuardStub{},
		Material: &Material{
			Card:    CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "01", ExpiryYear: "2030"},
			Billing: BillingAddress{Name: "Synthetic Holder", Line1: "1 Test Street", Line2: "Suite 2", City: "Test City", Country: "GB", PostalCode: "SW1A 1AA"},
		},
	}
}

func TestCheckoutAcceptsStructuredBillingWithoutCountryOrStateSubstitution(t *testing.T) {
	for _, country := range []string{"GB", "HK", "US"} {
		t.Run(country, func(t *testing.T) {
			request := syntheticBillingRequest()
			request.Material.Billing.Country = country
			request.Material.Billing.State = ""
			request.Material.Billing.PostalCode = ""
			if err := validateRequest(request); err != nil {
				t.Fatalf("structured billing rejected: %v", err)
			}
			if request.Material.Billing.Country != country || request.Material.Billing.State != "" || request.Material.Billing.PostalCode != "" {
				t.Fatal("checkout fabricated or replaced billing facts")
			}
			if request.PriceContract.Currency != "PHP" {
				t.Fatal("billing country altered the price contract")
			}
		})
	}
}

func TestCheckoutRejectsMalformedBillingIncludingLine2(t *testing.T) {
	for name, mutate := range map[string]func(*BillingAddress){
		"missing name":           func(b *BillingAddress) { b.Name = "" },
		"missing line1":          func(b *BillingAddress) { b.Line1 = "" },
		"missing city":           func(b *BillingAddress) { b.City = "" },
		"missing country":        func(b *BillingAddress) { b.Country = "" },
		"non-normalized country": func(b *BillingAddress) { b.Country = "gb" },
		"line2 control":          func(b *BillingAddress) { b.Line2 = "synthetic-private-marker\n" },
		"state control":          func(b *BillingAddress) { b.State = "synthetic-private-marker\n" },
	} {
		t.Run(name, func(t *testing.T) {
			request := syntheticBillingRequest()
			mutate(&request.Material.Billing)
			if err := validateRequest(request); err == nil {
				t.Fatal("malformed billing was accepted")
			}
		})
	}
}

func TestFilledMaterialCannotDropNonemptyLine2(t *testing.T) {
	material := *syntheticBillingRequest().Material
	values := checkoutFillValues(material)
	if values["billingLine2"] != material.Billing.Line2 {
		t.Fatal("line2 was not retained in the fill fragment")
	}
	filled := map[string]bool{"cardNumber": true, "cvc": true, "expiry": true, "billingName": true, "billingCountry": true}
	if err := validateFilledMaterial(material, filled); err == nil {
		t.Fatal("nonempty line2 was silently discarded")
	}
	filled["billingLine2"] = true
	if err := validateFilledMaterial(material, filled); err != nil {
		t.Fatalf("verified line2 did not satisfy the fill contract: %v", err)
	}
	material.Billing.Line2 = ""
	delete(filled, "billingLine2")
	if err := validateFilledMaterial(material, filled); err != nil {
		t.Fatalf("globally required optional state/postal/line2: %v", err)
	}
}

func TestFillReportRefusalOrRejectedOptionalFieldStopsGoCheckout(t *testing.T) {
	values := checkoutFillValues(*syntheticBillingRequest().Material)
	for name, result := range map[string]fillResult{
		"refused":                    {Accepted: false, Filled: []string{"cardNumber"}, Rejected: []string{}},
		"rejected line2":             {Accepted: true, Filled: []string{"cardNumber"}, Rejected: []string{"billingLine2"}},
		"rejected state":             {Accepted: true, Filled: []string{"cardNumber"}, Rejected: []string{"billingState"}},
		"malformed":                  {Accepted: true, Filled: []string{"cardNumber"}},
		"unexpected field":           {Accepted: true, Filled: []string{"unsupported"}, Rejected: []string{}},
		"empty value claimed filled": {Accepted: true, Filled: []string{"billingState"}, Rejected: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := collectFilledFields(result, values, map[string]bool{}); err == nil {
				t.Fatal("unsafe fill report was accepted")
			}
		})
	}
}

func TestCheckoutStateDoesNotRequireStateAndPostalControlsForEveryCountry(t *testing.T) {
	request := syntheticBillingRequest()
	amount := 1100.0
	page := inspectedPage{PageFacts: PageFacts{
		Origin: "https://pay.openai.com", RouteTemplate: "/pay/{id}", Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
		Fields:   map[string]bool{"cardNumber": true, "cvc": true, "expiry": true, "billingName": true, "billingCountry": true, "billingLine1": true, "billingCity": true},
		Controls: map[string]string{"submit": "payment-submit"},
	}}
	if got := classify(page, request, "checkout"); got != "PAYMENT_FINAL_READY" {
		t.Fatalf("state without optional controls = %s", got)
	}
	page.Country = "GB"
	if got := classify(page, request, "checkout"); got != "UNKNOWN_PAYMENT_STATE" {
		t.Fatal("billing support loosened the PH/PHP price contract")
	}
}
