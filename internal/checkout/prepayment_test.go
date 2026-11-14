package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/sessionrestore"
)

type prepaymentFake struct {
	observations                                                []prepaymentObservation
	observed                                                    int
	steps                                                       []string
	login                                                       sessionrestore.Report
	loginErr, openErr, billingErr, fillErr, verifyErr, closeErr error
	verifyFailAt                                                int
	verified                                                    int
	identity                                                    sessionrestore.Identity
	fenceState                                                  submissionState
	fenceSignal                                                 chan struct{}
}

func (fake *prepaymentFake) SubmissionState() submissionState  { return fake.fenceState }
func (fake *prepaymentFake) SubmissionSignal() <-chan struct{} { return fake.fenceSignal }

func (fake *prepaymentFake) Restore(context.Context, sessionrestore.Prepared) (sessionrestore.Report, error) {
	fake.steps = append(fake.steps, "restore")
	return fake.login, fake.loginErr
}
func (fake *prepaymentFake) OpenPlus(_ context.Context, identity sessionrestore.Identity) error {
	fake.steps = append(fake.steps, "open")
	fake.identity = identity
	return fake.openErr
}
func (fake *prepaymentFake) Observe(context.Context) (prepaymentObservation, error) {
	fake.steps = append(fake.steps, "observe")
	index := fake.observed
	fake.observed++
	if index >= len(fake.observations) {
		index = len(fake.observations) - 1
	}
	return fake.observations[index], nil
}
func (fake *prepaymentFake) FillBilling(context.Context, prepaymentObservation, BillingAddress) error {
	fake.steps = append(fake.steps, "billing")
	return fake.billingErr
}
func (fake *prepaymentFake) Fill(context.Context, prepaymentObservation, Material) error {
	fake.steps = append(fake.steps, "fill")
	return fake.fillErr
}
func (fake *prepaymentFake) Verify(context.Context, prepaymentObservation, Material) error {
	fake.steps = append(fake.steps, "verify")
	fake.verified++
	if fake.verifyFailAt == 0 || fake.verified == fake.verifyFailAt {
		return fake.verifyErr
	}
	return nil
}
func (fake *prepaymentFake) Close() error {
	fake.steps = append(fake.steps, "close")
	return fake.closeErr
}

func prepaymentFixture(t *testing.T) (PrepaymentRequest, *prepaymentFake) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"sessionToken": strings.Repeat("synthetic-token-", 5), "expires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "user": map[string]string{"id": "synthetic-user", "email": "fixture@example.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	subtotal, tax, discount, total := "1000.00", "120.00", "0.00", "1120.00"
	included := false
	observation := prepaymentObservation{Evidence: QuoteEvidence{Plan: "plus", Country: "PH", Currency: "PHP", Subtotal: &subtotal, Tax: &tax, Discount: &discount, Total: &total, TaxIncluded: &included}, EvidenceSource: "visible_dom", FinalControl: true, FinalEnabled: true}
	material := &Material{Card: CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "12", ExpiryYear: "2035"}, Billing: BillingAddress{Name: "Synthetic Holder", Line1: "1 Test Road", City: "Test City", Country: "US", State: "NY", PostalCode: "10001"}}
	request := PrepaymentRequest{Session: raw, Expected: QuoteTarget{Plan: "plus", Country: "PH", Currency: "PHP"}, Budget: QuoteBudget{Min: "1000.00", Max: "1300.00"}, Material: material}
	fake := &prepaymentFake{observations: []prepaymentObservation{observation}, login: sessionrestore.Report{RemoteVerified: true, CookieReadbackVerified: true, IdentityMatch: true, TargetFingerprint: "fixture-fingerprint"}, fenceState: submissionState{Ready: true}}
	return request, fake
}

func runPrepaymentFake(request PrepaymentRequest, fake *prepaymentFake) (PrepaymentReport, error) {
	return runPrepayment(context.Background(), request, time.Second, func(context.Context) (prepaymentDriver, error) { return fake, nil })
}

func TestPrepaymentReusesOwnedSessionUntilFinalReadbackAndNeverSubmits(t *testing.T) {
	request, fake := prepaymentFixture(t)
	callback := 0
	request.OnReady = func(report PrepaymentReport) {
		callback++
		if report.BrowserClosed || !report.ReadyForConfirmation || report.PaymentAllowed || report.SubmitActivated {
			t.Fatal("invalid review callback boundary")
		}
		if fake.steps[len(fake.steps)-1] != "verify" {
			t.Fatal("review preceded the final field readback")
		}
	}
	report, err := runPrepaymentFake(request, fake)
	if err != nil {
		t.Fatal(err)
	}
	if !report.ReadyForConfirmation || !report.FieldsVerified || !report.BillingVerified || !report.IdentityMatch || !report.CookieReadbackVerified || !report.BrowserClosed || report.PaymentAllowed || report.SubmitActivated || callback != 1 {
		t.Fatalf("incomplete report: %+v", report)
	}
	if fake.identity != (sessionrestore.Identity{UserID: "synthetic-user", Email: "fixture@example.invalid"}) {
		t.Fatal("identity was not bound to checkout creation")
	}
	want := []string{"restore", "open", "observe", "billing", "observe", "fill", "observe", "verify", "observe", "verify", "close"}
	if !reflect.DeepEqual(fake.steps, want) {
		t.Fatalf("steps=%v", fake.steps)
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{"synthetic-token", "fixture@example.invalid", "4242424242424242", "Synthetic Holder", "1 Test Road"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("report leaked protected material")
		}
	}
}

func TestPrepaymentObservationRejectsUnstableCompleteAndStaleQuotes(t *testing.T) {
	for _, scenario := range []string{"complete but not stable", "last read failed"} {
		t.Run(scenario, func(t *testing.T) {
			_, fake := prepaymentFixture(t)
			observation := fake.observations[0]
			observation.Page.Fields = map[string]bool{"number": true}
			calls := 0
			timeout := time.Millisecond
			if scenario == "last read failed" {
				timeout = pagePollInterval + 50*time.Millisecond
			}
			_, err := waitPrepaymentObservation(context.Background(), timeout, func() bool { return true }, func(context.Context) (prepaymentObservation, error) {
				calls++
				if scenario == "last read failed" && calls > 1 {
					return prepaymentObservation{}, errors.New("synthetic renderer detached")
				}
				return observation, nil
			})
			if err == nil || SafePrepaymentCode(err) != "PREPAYMENT_PAGE_UNSUPPORTED" {
				t.Fatalf("unconfirmed quote accepted: %v", err)
			}
			if scenario == "last read failed" && calls < 2 {
				t.Fatal("fixture did not exercise a failed newer read")
			}
		})
	}
}

func TestPrepaymentObservationCanExplainFreshMissingFacts(t *testing.T) {
	_, fake := prepaymentFixture(t)
	observation := fake.observations[0]
	observation.Evidence.Tax = nil
	observation.Missing = missingPrepaymentFacts(observation.Evidence)
	result, err := waitPrepaymentObservation(context.Background(), time.Millisecond, func() bool { return true }, func(context.Context) (prepaymentObservation, error) {
		return observation, nil
	})
	if err != nil || len(result.Missing) != 1 || result.Missing[0] != "tax" {
		t.Fatalf("fresh incomplete evidence was lost: %v %+v", err, result.Missing)
	}
}

func TestPrepaymentBillingCanUnlockTaxBeforeCardMaterial(t *testing.T) {
	request, fake := prepaymentFixture(t)
	material := request.Material
	request.Material = nil
	partial := fake.observations[0]
	partial.Evidence.Tax = nil
	partial.Evidence.Total = nil
	partial.Evidence.TaxIncluded = nil
	partial.Missing = missingPrepaymentFacts(partial.Evidence)
	fake.observations = []prepaymentObservation{partial, fake.observations[0]}
	request.ResolveBilling = func(context.Context) (*BillingAddress, error) {
		fake.steps = append(fake.steps, "resolve-billing")
		copy := material.Billing
		return &copy, nil
	}
	request.ResolveMaterial = func(_ context.Context, quote ReviewedQuote) (*Material, error) {
		if quote.TotalMinor != 112000 || fake.steps[len(fake.steps)-1] != "observe" {
			t.Fatal("card material requested before full quote")
		}
		fake.steps = append(fake.steps, "resolve-material")
		return material, nil
	}
	report, err := runPrepaymentFake(request, fake)
	if err != nil || !report.ReadyForConfirmation {
		t.Fatalf("billing-first failed: %v %+v", err, report)
	}
	if strings.Join(fake.steps, ",") != "restore,open,observe,resolve-billing,billing,observe,resolve-material,fill,observe,verify,observe,verify,close" {
		t.Fatal(fake.steps)
	}
}

func TestPrepaymentIncompleteOrWrongQuoteNeverRequestsCardMaterial(t *testing.T) {
	for _, name := range []string{"missing tax", "missing discount", "wrong currency", "outside budget"} {
		t.Run(name, func(t *testing.T) {
			request, fake := prepaymentFixture(t)
			request.Material = nil
			called := false
			request.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) { called = true; return nil, nil }
			switch name {
			case "missing tax":
				fake.observations[0].Evidence.Tax = nil
			case "missing discount":
				fake.observations[0].Evidence.Discount = nil
			case "wrong currency":
				fake.observations[0].Evidence.Currency = "EGP"
			case "outside budget":
				request.Budget.Max = "1100.00"
			}
			fake.observations[0].Missing = missingPrepaymentFacts(fake.observations[0].Evidence)
			report, err := runPrepaymentFake(request, fake)
			if err == nil || called || report.FieldsVerified || report.ReadyForConfirmation || !report.BrowserClosed {
				t.Fatalf("unsafe continuation: %v %+v", err, report)
			}
			if strings.Contains(strings.Join(fake.steps, ","), "fill") {
				t.Fatal("filled before quote review")
			}
		})
	}
}

func TestPrepaymentBindingAndMaterialFailuresStopBeforeFilling(t *testing.T) {
	for _, name := range []string{"login mismatch", "login unconfirmed", "billing changed", "malformed card", "material error"} {
		t.Run(name, func(t *testing.T) {
			request, fake := prepaymentFixture(t)
			material := *request.Material
			switch name {
			case "login mismatch":
				fake.loginErr = prepaymentFail("PREPAYMENT_IDENTITY_CHANGED")
			case "login unconfirmed":
				fake.login.RemoteVerified = false
			case "billing changed":
				request.Material = nil
				request.ResolveBilling = func(context.Context) (*BillingAddress, error) { copy := material.Billing; return &copy, nil }
				request.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) {
					material.Billing.City = "Other City"
					return &material, nil
				}
			case "malformed card":
				request.Material = nil
				request.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) {
					material.Card.ExpiryMonth = "13"
					return &material, nil
				}
			case "material error":
				request.Material = nil
				request.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) {
					return nil, errors.New("PRIVATE_TOKEN_SENTINEL")
				}
			}
			report, err := runPrepaymentFake(request, fake)
			if err == nil || report.ReadyForConfirmation || !report.BrowserClosed || strings.Contains(err.Error(), "PRIVATE_TOKEN_SENTINEL") {
				t.Fatalf("unsafe failure: %v %+v", err, report)
			}
			for _, step := range fake.steps {
				if step == "fill" {
					t.Fatal("filled card after failure")
				}
			}
		})
	}
}

func TestPrepaymentProgressionDisabledControlAndLateResetCannotBeReady(t *testing.T) {
	for _, name := range []string{"progression", "disabled submit", "reset on final read", "quote changed"} {
		t.Run(name, func(t *testing.T) {
			request, fake := prepaymentFixture(t)
			want := ""
			switch name {
			case "progression":
				fake.observations[0].Progression = true
				fake.observations[0].FinalControl = false
				want = "PREPAYMENT_PROGRESSION_REQUIRED"
			case "disabled submit":
				fake.observations[0].FinalEnabled = false
				want = "PREPAYMENT_FINAL_CONTROL_UNCONFIRMED"
			case "reset on final read":
				fake.verifyErr = errors.New("reset")
				fake.verifyFailAt = 2
				want = "PREPAYMENT_FIELDS_NOT_VERIFIED"
			case "quote changed":
				initial := fake.observations[0]
				changed := initial
				total := "1200.00"
				tax := "200.00"
				changed.Evidence.Total = &total
				changed.Evidence.Tax = &tax
				fake.observations = []prepaymentObservation{initial, initial, initial, changed}
				want = "PREPAYMENT_QUOTE_CHANGED"
			}
			report, err := runPrepaymentFake(request, fake)
			if err == nil || report.Code != want || report.ReadyForConfirmation || report.PaymentAllowed || report.SubmitActivated || !report.BrowserClosed {
				t.Fatalf("unsafe ready: %v %+v", err, report)
			}
		})
	}
}

func TestPrepaymentPriceChangeAfterCardFillRequiresAnotherReview(t *testing.T) {
	request, fake := prepaymentFixture(t)
	initial := fake.observations[0]
	changed := initial
	tax, total := "125.00", "1125.00"
	changed.Evidence.Tax = &tax
	changed.Evidence.Total = &total
	fake.observations = []prepaymentObservation{initial, initial, changed, changed}
	report, err := runPrepaymentFake(request, fake)
	if err == nil || report.Quote.TotalMinor != 112500 || report.ReadyForConfirmation || report.Code != "PREPAYMENT_QUOTE_CHANGED" {
		t.Fatalf("changed price silently accepted: %v %+v", err, report)
	}
}

func TestPrepaymentUnsupportedInputCannotStartBrowser(t *testing.T) {
	request, _ := prepaymentFixture(t)
	for _, change := range []func(*PrepaymentRequest){
		func(r *PrepaymentRequest) { r.Expected.Plan = "go" },
		func(r *PrepaymentRequest) { r.Expected.Country = "EG"; r.Expected.Currency = "EGP" },
		func(r *PrepaymentRequest) { r.Budget.Min = "0" },
		func(r *PrepaymentRequest) {
			r.ResolveMaterial = func(context.Context, ReviewedQuote) (*Material, error) { return nil, nil }
		},
		func(r *PrepaymentRequest) { r.Session = json.RawMessage(`{"accessToken":"only"}`) },
	} {
		copy := request
		change(&copy)
		started := false
		report, err := runPrepayment(context.Background(), copy, time.Second, func(context.Context) (prepaymentDriver, error) { started = true; return nil, nil })
		if err == nil || started || report.BrowserOpened || report.ReadyForConfirmation {
			t.Fatal("invalid input started browser")
		}
	}
}

func TestPrepaymentCleanupFailureRevokesReadyReport(t *testing.T) {
	request, fake := prepaymentFixture(t)
	fake.closeErr = errors.New("PRIVATE_PROFILE_PATH")
	report, err := runPrepaymentFake(request, fake)
	if err == nil || report.Code != "PREPAYMENT_CLEANUP_FAILED" || report.BrowserClosed || report.ReadyForConfirmation || strings.Contains(err.Error(), "PRIVATE_PROFILE_PATH") {
		t.Fatalf("bad cleanup report: %v %+v", err, report)
	}
}

func TestPrepaymentObservationDoesNotTrustInjectedLegacyPriceFacts(t *testing.T) {
	request, fake := prepaymentFixture(t)
	_ = request
	frames := []frameFact{{Origin: "https://pay.openai.com", Plan: "plus", Currency: "PHP", Country: "PH"}, {Origin: "https://foreign.invalid", Fields: map[string]bool{"cardNumber": true}}}
	observation, err := observationFromDOM(prepaymentDOM{Origin: "https://pay.openai.com", Source: "visible_dom"}, frames, "https://pay.openai.com", "/pay/{id}")
	if err != nil {
		t.Fatal(err)
	}
	if observation.Evidence.Plan != "" || observation.Evidence.Currency != "" || len(observation.Missing) == 0 || observation.Page.Fields["cardNumber"] {
		t.Fatal("legacy/foreign facts were trusted")
	}
	if _, err := reviewedPrepaymentQuote(PrepaymentRequest{Expected: QuoteTarget{Plan: "plus", Country: "PH", Currency: "PHP"}, Budget: QuoteBudget{Min: "1000", Max: "1200"}}, observation); err == nil {
		t.Fatal("missing DOM facts were accepted")
	}
	_ = fake
}

func TestBoundPlusPreparationBindsIdentityWithoutTextualGuardInsertion(t *testing.T) {
	expression, err := bindPlusPreparation(sessionrestore.Identity{Email: " Fixture@Example.invalid ", UserID: "synthetic-user"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(expression, "__PREPAYMENT_IDENTITY__") ||
		strings.Count(expression, `{"Email":"fixture@example.invalid","UserID":"synthetic-user"}`) != 1 ||
		expression == preparePlusCheckoutJS {
		t.Fatal("isolated entry did not bind the normalized identity exactly once")
	}
}
