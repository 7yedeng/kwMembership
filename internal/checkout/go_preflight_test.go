package checkout

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

func syntheticGoObservation() Request {
	return Request{
		Mode: ModeInteractivePreflight, Stage: "go", TargetTier: "go", ExpectedEmail: "buyer@example.invalid",
		PriceContract: PriceContract{ID: "synthetic-go", Version: 1, Tier: "go", Currency: "PHP", MinAmount: 100, MaxAmount: 200},
		OnHandoff:     func(context.Context, Handoff) error { return nil },
	}
}

func TestGoExecutorAllowsOnlyUnboundInteractiveObservation(t *testing.T) {
	request := syntheticGoObservation()
	if err := validateRequest(request); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"Plus stage":         func(r *Request) { r.Stage = "plus" },
		"upgrade stage":      func(r *Request) { r.Stage = "upgrade" },
		"Plus price":         func(r *Request) { r.PriceContract.Tier = "plus" },
		"raw credentials":    func(r *Request) { r.Session = json.RawMessage(`{"accessToken":"synthetic"}`) },
		"workflow binding":   func(r *Request) { r.Binding.FulfillmentID = "synthetic-job" },
		"card material":      func(r *Request) { r.Material = syntheticBillingRequest().Material },
		"payment guard":      func(r *Request) { r.Guard = requestGuardStub{} },
		"Session preflight":  func(r *Request) { r.Mode = ModeSessionPreflight },
		"Session checkout":   func(r *Request) { r.Mode = ModeSessionCheckout },
		"prepared preflight": func(r *Request) { r.Mode = ModePreflight },
		"prepared checkout":  func(r *Request) { r.Mode = ModeCheckout },
		"automatic upgrade":  func(r *Request) { r.Mode = ModeUpgrade },
		"NaN price minimum":  func(r *Request) { r.PriceContract.MinAmount = math.NaN() },
		"infinite price cap": func(r *Request) { r.PriceContract.MaxAmount = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			mutate(&candidate)
			if err := validateRequest(candidate); err == nil {
				t.Fatal("Go observation escaped its target or non-payment boundary")
			}
		})
	}
}

func TestInteractiveIdentityRequiresCurrentSuccessfulObservation(t *testing.T) {
	const expected = "buyer@example.invalid"
	for _, tc := range []struct {
		name      string
		identity  authenticatedIdentity
		verified  bool
		errorCode string
	}{
		{"matching", authenticatedIdentity{Email: expected}, true, ""},
		{"matching with refresh error", authenticatedIdentity{Email: expected, ErrorKind: "RefreshAccessTokenError"}, false, "INTERACTIVE_LOGIN_IDENTITY_UNVERIFIED"},
		{"missing observation", authenticatedIdentity{}, false, ""},
		{"another account", authenticatedIdentity{Email: "other@example.invalid"}, false, "INTERACTIVE_LOGIN_IDENTITY_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verified, err := interactiveIdentityVerified(tc.identity, expected)
			if verified != tc.verified {
				t.Fatal("incorrect current identity evidence")
			}
			if tc.errorCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if coded, ok := err.(*Error); !ok || coded.Code() != tc.errorCode {
				t.Fatalf("wrong identity error: %v", err)
			}
		})
	}
}

func TestGoFactsDoNotSatisfyPlusAndPlusFactsDoNotSatisfyGo(t *testing.T) {
	amount := 150.0
	request := syntheticGoObservation()
	page := inspectedPage{PageFacts: PageFacts{
		Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}", Plan: "go", Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
		Fields:   map[string]bool{"cardNumber": true, "cvc": true, "expiry": true, "billingName": true, "billingCountry": true},
		Controls: map[string]string{"submit": "payment-submit"},
	}}
	if got := classify(page, request, "checkout"); got != "PAYMENT_FINAL_READY" {
		t.Fatalf("explicit synthetic Go facts were not recognized: %s", got)
	}
	page.Plan = "plus"
	if got := classify(page, request, "checkout"); got != "UNKNOWN_PAYMENT_STATE" {
		t.Fatal("Plus page satisfied Go observation")
	}
	page.Plan = "go"
	request.Stage, request.TargetTier, request.PriceContract.Tier = "plus", "plus", "plus"
	if got := classify(page, request, "checkout"); got != "UNKNOWN_PAYMENT_STATE" {
		t.Fatal("Go page satisfied Plus observation")
	}
	request = syntheticGoObservation()
	request.Mode = ModeSessionCheckout
	if got := classify(page, request, "checkout"); got != "UNKNOWN_PAYMENT_STATE" {
		t.Fatal("Go facts authorized a payment-capable mode")
	}
}

func TestPaymentContractMustMatchCurrentStageAndCannotSmuggleGo(t *testing.T) {
	for _, target := range []string{"plus", "x5", "x20"} {
		t.Run(target, func(t *testing.T) {
			request := syntheticBillingRequest()
			request.TargetTier = target
			if err := validateRequest(request); err != nil {
				t.Fatalf("valid initial Plus stage of a higher-tier order was rejected: %v", err)
			}
			request.PriceContract.Tier = "go"
			if err := validateRequest(request); err == nil {
				t.Fatal("payment mode accepted a Go contract under a Plus target/stage")
			}
			request.PriceContract.Tier = "plus"
			request.Mode = ModeSessionPreflight
			request.Material, request.Guard = nil, nil
			if err := validateRequest(request); err != nil {
				t.Fatalf("valid initial Plus preflight of a higher-tier order was rejected: %v", err)
			}
			if target != "plus" {
				request = syntheticBillingRequest()
				request.TargetTier, request.Stage, request.PriceContract.Tier = target, "upgrade", target
				request.PlanURL = PlanManagementURL
				if err := validateRequest(request); err != nil {
					t.Fatalf("valid upgrade price contract was rejected: %v", err)
				}
				request.PriceContract.Tier = "go"
				if err := validateRequest(request); err == nil {
					t.Fatal("upgrade mode accepted a Go contract")
				}
			}
		})
	}
}
