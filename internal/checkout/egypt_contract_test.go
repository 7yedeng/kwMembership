package checkout

import (
	"encoding/json"
	"testing"
)

func syntheticEgyptGoRequest() Request {
	request := syntheticGoObservation()
	request.PriceContract.Country = "EG"
	request.PriceContract.Currency = "EGP"
	return request
}

func TestEgyptContractIsExplicitAndOnlyForInteractiveGo(t *testing.T) {
	request := syntheticEgyptGoRequest()
	if err := validateRequest(request); err != nil {
		t.Fatalf("valid synthetic Egypt Go observation was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Request){
		"missing country":     func(r *Request) { r.PriceContract.Country = "" },
		"PH EGP mismatch":     func(r *Request) { r.PriceContract.Country = "PH" },
		"EG PHP mismatch":     func(r *Request) { r.PriceContract.Currency = "PHP" },
		"unrecognized market": func(r *Request) { r.PriceContract.Country = "US"; r.PriceContract.Currency = "USD" },
		"Plus observation": func(r *Request) {
			r.Stage, r.TargetTier, r.PriceContract.Tier = "plus", "plus", "plus"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			mutate(&candidate)
			if err := ValidatePlanContract(candidate); err == nil {
				t.Fatal("unsupported country/currency pair or target was accepted")
			}
		})
	}
	for _, mode := range []Mode{ModePreflight, ModeSessionPreflight, ModeSessionCheckout, ModeCheckout, ModeUpgrade} {
		t.Run(string(mode), func(t *testing.T) {
			candidate := request
			candidate.Mode = mode
			if err := ValidatePlanContract(candidate); err == nil {
				t.Fatal("Egypt Go parameters enabled a non-interactive mode")
			}
			// Also test an otherwise-supported plan/stage. Rejecting only Go
			// would still let EG/EGP leak into automatic Plus/upgrade flows.
			candidate.Stage, candidate.TargetTier, candidate.PriceContract.Tier = "plus", "plus", "plus"
			if mode == ModeUpgrade {
				candidate.Stage, candidate.TargetTier, candidate.PriceContract.Tier = "upgrade", "x5", "x5"
			}
			if err := ValidatePlanContract(candidate); err == nil {
				t.Fatal("Egypt parameters widened the existing payment market contract")
			}
		})
	}
}

func TestLegacyPHContractDefaultsAndJSONRemainCompatible(t *testing.T) {
	for _, country := range []string{"", "PH"} {
		request := syntheticGoObservation()
		request.PriceContract.Country = country
		if err := validateRequest(request); err != nil {
			t.Fatalf("legacy PH/PHP Go observation was rejected: %v", err)
		}
		payment := syntheticBillingRequest()
		payment.PriceContract.Country = country
		if err := validateRequest(payment); err != nil {
			t.Fatalf("legacy PH/PHP payment request changed: %v", err)
		}
	}
	contract := syntheticGoObservation().PriceContract
	encoded, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["Country"]; exists {
		t.Fatal("empty Country changed the existing bridge JSON shape")
	}
	if _, exists := payload["country"]; exists {
		t.Fatal("empty country changed the existing bridge JSON shape")
	}
	contract.Country, contract.Currency = "EG", "EGP"
	encoded, err = json.Marshal(contract)
	if err != nil || json.Unmarshal(encoded, &payload) != nil || payload["Country"] != "EG" || payload["Currency"] != "EGP" {
		t.Fatal("explicit country was not preserved alongside the existing capitalized contract fields")
	}
}

func TestEgyptGoPageFactsCannotSatisfyAnotherMarketOrAutomaticMode(t *testing.T) {
	request := syntheticEgyptGoRequest()
	// Synthetic fixture only: 193 is not a verified live quote or an
	// authorization to create a Go SKU or submit any payment.
	amount := 193.0
	base := PageFacts{
		Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}", Plan: "go", Country: "EG", Currency: "EGP", DisplayedAmount: &amount,
		Fields:   map[string]bool{"cardNumber": true, "cvc": true, "expiry": true, "billingName": true, "billingCountry": true},
		Controls: map[string]string{"submit": "payment-submit"},
	}
	if got := classify(inspectedPage{PageFacts: base}, request, "checkout"); got != "PAYMENT_FINAL_READY" {
		t.Fatalf("explicit synthetic Egypt Go facts were not recognized: %s", got)
	}
	for name, mutate := range map[string]func(*PageFacts){
		"wrong plan":        func(p *PageFacts) { p.Plan = "plus" },
		"wrong country":     func(p *PageFacts) { p.Country = "PH" },
		"wrong currency":    func(p *PageFacts) { p.Currency = "PHP" },
		"unknown country":   func(p *PageFacts) { p.Country = "" },
		"unknown currency":  func(p *PageFacts) { p.Currency = "" },
		"other price range": func(p *PageFacts) { outside := 201.0; p.DisplayedAmount = &outside },
	} {
		t.Run(name, func(t *testing.T) {
			page := base
			mutate(&page)
			if classify(inspectedPage{PageFacts: page}, request, "checkout") != "UNKNOWN_PAYMENT_STATE" {
				t.Fatal("mismatched Egypt Go page facts satisfied the contract")
			}
		})
	}
	for _, mode := range []Mode{ModePreflight, ModeSessionPreflight, ModeSessionCheckout, ModeCheckout, ModeUpgrade} {
		candidate := request
		candidate.Mode = mode
		if classify(inspectedPage{PageFacts: base}, candidate, "checkout") != "UNKNOWN_PAYMENT_STATE" {
			t.Fatal("Egypt Go facts reached an automatic observation/payment classifier")
		}
		candidate.Stage, candidate.TargetTier, candidate.PriceContract.Tier = "plus", "plus", "plus"
		page := base
		page.Plan = "plus"
		if mode == ModeUpgrade {
			candidate.Stage, candidate.TargetTier, candidate.PriceContract.Tier = "upgrade", "x5", "x5"
			page.Plan = "prolite"
		}
		if classify(inspectedPage{PageFacts: page}, candidate, "checkout") != "UNKNOWN_PAYMENT_STATE" {
			t.Fatal("Egypt market facts widened a non-interactive Plus/upgrade classifier")
		}
	}
}
