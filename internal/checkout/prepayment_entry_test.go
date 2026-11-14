package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func confirmedAccountDiagnostic() *CheckoutDiagnostic {
	return &CheckoutDiagnostic{Phase: "complete", Reason: "checkout_created", Source: "account_entitlement",
		SessionHTTPStatus: 200, AccountHTTPStatus: 200, CheckoutHTTPStatus: 200,
		AccountIDPresent: true, AccountBinding: "matched", Plan: "free", ActiveSubscription: "false", CheckoutRequested: true}
}

func TestCheckoutDiagnosticRequiresFreshBoundFreeEvidence(t *testing.T) {
	good := confirmedAccountDiagnostic()
	if !good.confirmsFreeCheckout() || (*CheckoutDiagnostic)(nil).confirmsFreeCheckout() {
		t.Fatal("valid and absent entry evidence were not distinguished")
	}
	for name, change := range map[string]func(*CheckoutDiagnostic){
		"unknown plan":              func(v *CheckoutDiagnostic) { v.Plan = "unknown" },
		"active subscription":       func(v *CheckoutDiagnostic) { v.ActiveSubscription = "true" },
		"invalid active status":     func(v *CheckoutDiagnostic) { v.ActiveSubscription = "unexpected" },
		"missing account":           func(v *CheckoutDiagnostic) { v.AccountIDPresent = false },
		"unbound account":           func(v *CheckoutDiagnostic) { v.AccountBinding = "none" },
		"unauthorized account read": func(v *CheckoutDiagnostic) { v.AccountHTTPStatus = 403 },
		"unverified session":        func(v *CheckoutDiagnostic) { v.SessionHTTPStatus = 401 },
		"checkout not requested":    func(v *CheckoutDiagnostic) { v.CheckoutRequested = false },
		"checkout rejected":         func(v *CheckoutDiagnostic) { v.CheckoutHTTPStatus = 403 },
		"checkout timeout":          func(v *CheckoutDiagnostic) { v.Reason = "checkout_timeout" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *good
			change(&changed)
			if changed.confirmsFreeCheckout() {
				t.Fatal("incomplete or conflicting evidence was accepted")
			}
		})
	}
	legacy := *good
	legacy.Source, legacy.AccountBinding = "subscription", "session"
	legacy.AccountHTTPStatus, legacy.SubscriptionHTTPStatus = 404, 200
	if !legacy.confirmsFreeCheckout() {
		t.Fatal("explicit supported legacy fallback was rejected")
	}
	legacy.AccountHTTPStatus = 403
	if legacy.confirmsFreeCheckout() {
		t.Fatal("an access refusal was treated as an unsupported-endpoint fallback")
	}
}

func TestCheckoutDiagnosticScrubsUnknownStringsAndStatusCodes(t *testing.T) {
	const private = "private-token-at-https://private.invalid/?account=hidden"
	input := &CheckoutDiagnostic{Phase: private, Reason: private, Source: private,
		SessionHTTPStatus: -1, AccountHTTPStatus: 900, SubscriptionHTTPStatus: 42,
		CheckoutHTTPStatus: 1000, AccountBinding: private, Plan: private, ActiveSubscription: private}
	clean := SanitizeCheckoutDiagnostic(input)
	encoded, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{string(encoded), fmt.Sprintf("%+v %#v", clean, clean)} {
		if strings.Contains(value, private) {
			t.Fatal("raw diagnostic text escaped")
		}
	}
	if clean.Plan != "unknown" || clean.ActiveSubscription != "unknown" || clean.Source != "none" ||
		clean.SessionHTTPStatus != 0 || clean.AccountHTTPStatus != 0 || clean.SubscriptionHTTPStatus != 0 || clean.CheckoutHTTPStatus != 0 ||
		clean.confirmsFreeCheckout() || input.Reason != private {
		t.Fatal("diagnostic sanitization changed its source or invented positive evidence")
	}
}

type diagnosticPrepaymentFake struct {
	*prepaymentFake
	diagnostic *CheckoutDiagnostic
}

func (fake *diagnosticPrepaymentFake) CheckoutDiagnostic() *CheckoutDiagnostic {
	return fake.diagnostic
}

func TestEligibilityFailureRetainsDiagnosisBeforeAnyCardLookup(t *testing.T) {
	request, fake := prepaymentFixture(t)
	fake.openErr = prepaymentFail("PREPAYMENT_ELIGIBILITY_UNCONFIRMED")
	reads := 0
	request.Material = nil
	request.ResolveBilling = func(context.Context) (*BillingAddress, error) { reads++; return nil, nil }
	request.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) { reads++; return nil, nil }
	driver := &diagnosticPrepaymentFake{prepaymentFake: fake, diagnostic: &CheckoutDiagnostic{
		Phase: "account_lookup", Reason: "account_http_rejected", Source: "none",
		SessionHTTPStatus: 200, AccountHTTPStatus: 403, AccountIDPresent: true, AccountBinding: "session", Plan: "unknown", ActiveSubscription: "unknown"}}
	report, err := runPrepayment(context.Background(), request, time.Second, func(context.Context) (prepaymentDriver, error) { return driver, nil })
	if err == nil || report.Code != "PREPAYMENT_ELIGIBILITY_UNCONFIRMED" || report.Stage != "opening_checkout" ||
		report.CheckoutDiagnostic == nil || report.CheckoutDiagnostic.Reason != "account_http_rejected" ||
		report.CheckoutDiagnostic.AccountHTTPStatus != 403 || report.CheckoutDiagnostic.CheckoutRequested ||
		reads != 0 || !report.CookieReadbackVerified || !report.IdentityMatch || !report.BrowserClosed || report.ReadyForConfirmation ||
		!reflect.DeepEqual(fake.steps, []string{"restore", "open", "close"}) {
		t.Fatalf("eligibility failure advanced or lost safe facts: %+v", report)
	}
}
