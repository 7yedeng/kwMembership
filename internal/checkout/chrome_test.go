package checkout

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	cdpruntime "github.com/chromedp/cdproto/runtime"

	"kwmembership/internal/provider"
)

type requestGuardStub struct{}

func TestAuthenticatedIdentityEvaluationAwaitsPromise(t *testing.T) {
	params := cdpruntime.Evaluate(authenticatedEmailJS)
	for _, option := range authenticatedIdentityEvalOptions {
		params = option(params)
	}
	if !params.AwaitPromise {
		t.Fatal("async Session identity evaluation does not await its Promise")
	}
}

func (requestGuardStub) BeforeAction(context.Context, Action) (Permit, error) {
	return Permit{ID: "permit"}, nil
}
func (requestGuardStub) ActivateAction(context.Context, Action, Permit) error { return nil }
func (requestGuardStub) AfterAction(context.Context, Action, Permit) (ActionOutcome, error) {
	return ActionOutcome{Continue: true}, nil
}

func TestValidateRequestAllowsPasswordlessInteractivePreflightOnly(t *testing.T) {
	request := Request{
		Mode: ModeInteractivePreflight, Stage: "plus", TargetTier: "plus",
		ExpectedEmail: "buyer@example.com",
		PriceContract: PriceContract{ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200},
		OnHandoff:     func(context.Context, Handoff) error { return nil },
	}
	if err := validateRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Cookies = []provider.SessionCookie{{Name: "unexpected"}}
	if err := validateRequest(request); err == nil {
		t.Fatal("interactive preflight accepted injected cookies")
	}
	request.Cookies = nil
	request.ExpectedEmail = "not-an-email"
	if err := validateRequest(request); err == nil {
		t.Fatal("interactive preflight accepted an invalid expected identity")
	}
}

func TestValidateRequestAllowsSessionDrivenPreflightAndCheckout(t *testing.T) {
	base := Request{
		Mode: ModeSessionPreflight, Stage: "plus", TargetTier: "plus",
		ExpectedEmail: "buyer@example.com",
		Cookies:       []provider.SessionCookie{{Name: "__Secure-next-auth.session-token.0", Value: "opaque"}},
		PriceContract: PriceContract{ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200},
	}
	if err := validateRequest(base); err != nil {
		t.Fatal(err)
	}
	withURL := base
	withURL.CheckoutURL = "https://chatgpt.com/checkout/unsafe-prepared-entry"
	if err := validateRequest(withURL); err == nil {
		t.Fatal("Session preflight accepted a prepared Checkout URL")
	}
	checkoutRequest := base
	checkoutRequest.Mode = ModeSessionCheckout
	checkoutRequest.Material = &Material{
		Card:    CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "12", ExpiryYear: "2030"},
		Billing: BillingAddress{Name: "Buyer", Line1: "1 Main St", City: "Wilmington", State: "DE", PostalCode: "19801", Country: "US"},
	}
	checkoutRequest.Guard = requestGuardStub{}
	if err := validateRequest(checkoutRequest); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`/api/auth/session`,
		`/backend-api/subscriptions?`,
		`/backend-api/payments/checkout`,
		`billing_details: {country: 'PH', currency: 'PHP'}`,
		`checkout_ui_mode: 'hosted'`,
		`entry_point: 'all_plans_pricing_modal'`,
		`plan_name: 'chatgptplusplan'`,
	} {
		if !strings.Contains(preparePlusCheckoutJS, required) {
			t.Fatalf("direct Plus checkout script is missing %q", required)
		}
	}
	for _, forbidden := range []string{
		`/subscriptions/renew`,
		`select-plan-button-plus-upgrade`,
		`/#pricing`,
		`price_interval:`,
		`seat_quantity:`,
		`promo_campaign:`,
	} {
		if strings.Contains(preparePlusCheckoutJS, forbidden) {
			t.Fatalf("direct Plus checkout script contains forbidden legacy/mutating path %q", forbidden)
		}
	}
}

func TestResolvePlusCheckoutEntryAllowsOnlyReviewedResponseShapes(t *testing.T) {
	tests := []struct {
		name  string
		entry plusCheckoutEntry
		want  string
	}{
		{
			name:  "hosted",
			entry: plusCheckoutEntry{ResponseTag: "hosted_checkout_session", CheckoutURL: "https://pay.openai.com/c/pay/cs_safe"},
			want:  "https://pay.openai.com/c/pay/cs_safe",
		},
		{
			name: "custom oaics",
			entry: plusCheckoutEntry{
				ResponseTag: "custom_checkout_session", ProcessorEntity: "openai_llc", CheckoutSessionID: "oaics_safe",
			},
			want: "https://chatgpt.com/checkout/openai_llc/oaics_safe",
		},
		{
			name: "custom cs",
			entry: plusCheckoutEntry{
				ResponseTag: "custom_checkout_session", ProcessorEntity: "openai_llc", CheckoutSessionID: "cs_safe",
			},
			want: "https://chatgpt.com/checkout/openai_llc/cs_safe",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolvePlusCheckoutEntry(test.entry)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("checkout entry = %q, want %q", got, test.want)
			}
		})
	}

	for name, entry := range map[string]plusCheckoutEntry{
		"foreign URL": {
			ResponseTag: "hosted_checkout_session", CheckoutURL: "https://example.com/checkout/cs_unsafe",
		},
		"unknown tag": {
			ResponseTag: "unknown", CheckoutURL: "https://pay.openai.com/c/pay/cs_safe",
		},
		"foreign custom processor": {
			ResponseTag: "custom_checkout_session", ProcessorEntity: "other", CheckoutSessionID: "oaics_safe",
		},
		"wrong custom ID": {
			ResponseTag: "custom_checkout_session", ProcessorEntity: "openai_llc", CheckoutSessionID: "other_safe",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolvePlusCheckoutEntry(entry); err == nil {
				t.Fatal("unsafe checkout response was accepted")
			}
		})
	}
}

func TestRouteIDPrefixClassNeverReturnsSessionMaterial(t *testing.T) {
	if got := routeIDPrefixClass("cs_sensitive-material"); got != "cs_" {
		t.Fatalf("prefix class = %q", got)
	}
	for _, value := range []string{"", "_secret", "prefix-with-dash_secret", "verylongprefix_secret"} {
		if got := routeIDPrefixClass(value); got != "unclassified" {
			t.Fatalf("unsafe prefix class for %q = %q", value, got)
		}
	}
}

func TestMergeFactsRecognizesAllowlistedCheckoutAcrossFrames(t *testing.T) {
	amount := 1150.0
	request := Request{
		Mode: ModePreflight, Stage: "plus", TargetTier: "plus",
		PriceContract: PriceContract{ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200},
	}
	page, err := mergeFacts([]frameFact{
		{
			FrameID: "top", Origin: "https://chatgpt.com", RouteTemplate: "/checkout/{id}",
			Plan: "plus", Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
			Controls: map[string]string{"submit": "payment-submit"}, Fields: map[string]bool{},
		},
		{
			FrameID: "stripe", Origin: "https://js.stripe.com", Fields: map[string]bool{
				"cardNumber": true, "expiry": true, "cvc": true, "billingName": true,
				"billingCountry": true, "billingPostal": true,
			}, Controls: map[string]string{},
		},
	}, "https://chatgpt.com", "/checkout/{id}", request, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if page.StateID != "PAYMENT_FINAL_READY" || page.controlFrames["submit"] != cdp.FrameID("top") || len(page.fieldFrames) != 1 {
		t.Fatalf("unexpected checkout page: state=%s controls=%v fieldFrames=%d", page.StateID, page.controlFrames, len(page.fieldFrames))
	}
	if page.StructuralHash == "" {
		t.Fatal("missing structural hash")
	}
}

func TestAllowedCheckoutURLRecognizesOpenAILLCPath(t *testing.T) {
	for _, prefix := range []string{"oaics_", "cs_"} {
		parsed, err := allowedCheckoutURL("https://chatgpt.com/checkout/openai_llc/" + prefix + "4e33c980620248fb926f384591ae06f1")
		if err != nil {
			t.Fatal(err)
		}
		if got := routeTemplate(parsed); got != "/checkout/{id}" {
			t.Fatalf("routeTemplate() = %q", got)
		}
	}
	if _, err := allowedCheckoutURL("https://chatgpt.com/checkout/other_org/oaics_4e33c980620248fb926f384591ae06f1"); err == nil {
		t.Fatal("unexpected checkout organization accepted")
	}
}

func TestMergeFactsRejectsAmbiguousPaymentControls(t *testing.T) {
	amount := 1150.0
	request := Request{Mode: ModePreflight, Stage: "plus", TargetTier: "plus", PriceContract: PriceContract{
		ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200,
	}}
	page, err := mergeFacts([]frameFact{{
		FrameID: "top", Origin: "https://chatgpt.com", RouteTemplate: "/checkout", Plan: "plus",
		Country: "PH", Currency: "PHP", DisplayedAmount: &amount,
		Fields:   map[string]bool{"cardNumber": true, "expiry": true, "cvc": true, "billingName": true, "billingCountry": true, "billingPostal": true},
		Controls: map[string]string{"progression": "payment-next", "submit": "payment-submit"},
	}}, "https://chatgpt.com", "/checkout", request, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if page.StateID != "UNKNOWN_PAYMENT_STATE" {
		t.Fatalf("state = %s, want UNKNOWN_PAYMENT_STATE", page.StateID)
	}
}

func TestMergeFactsRecognizesCloudflareChallengeBeforeCheckoutSurface(t *testing.T) {
	request := Request{Mode: ModePreflight, Stage: "plus", TargetTier: "plus", PriceContract: PriceContract{
		ID: "price-1", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1100, MaxAmount: 1200,
	}}
	page, err := mergeFacts([]frameFact{{
		FrameID: "top", Origin: "https://chatgpt.com", RouteTemplate: "",
		Fields: map[string]bool{}, Controls: map[string]string{"challenge": "challenge-cloudflare"},
	}}, "https://chatgpt.com", "", request, "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if page.StateID != "PAYMENT_ACTION_REQUIRED" {
		t.Fatalf("state = %s, want PAYMENT_ACTION_REQUIRED", page.StateID)
	}
	if page.Controls["challenge"] != "challenge-cloudflare" {
		t.Fatalf("challenge = %q", page.Controls["challenge"])
	}
}
