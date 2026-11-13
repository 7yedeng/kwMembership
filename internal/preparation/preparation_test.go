package preparation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
)

var fixtureNow = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func inputJSON() []byte {
	return []byte(`{"session":{"sessionToken":"synthetic-session-credential-not-valid-anywhere-0123456789","user":{"id":"fixture-user","email":"fixture-owner@example.invalid"}},"target":{"plan":"plus","country":"PH","currency":"PHP"},"budget":{"min":"1000.00","max":"1200.00"},"card":{"platformKey":"zovo-fixture","kind":"zovocard","cardId":7,"credential":{"appId":"synthetic-app-id","appSecret":"synthetic-api-secret"}}}`)
}

func fixtureInput(t *testing.T) Input {
	t.Helper()
	input, err := Parse(inputJSON(), fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func fixtureSnapshot() provider.CardSnapshot {
	return provider.CardSnapshot{UpstreamCardID: 7, CardBindingVerified: true, AvailableAmount: 30,
		Material: provider.CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "12", ExpiryYear: "2035", Status: "ACTIVE",
			HolderName: "Fixture Owner", Billing: &provider.BillingAddress{Name: "Fixture Owner", Line1: "1 Fixture Road", City: "Fixture City", Country: "US"}}}
}

type snapshotReader struct {
	snapshot provider.CardSnapshot
	calls    int
	err      error
}

func (reader *snapshotReader) GetCardSnapshot(ctx context.Context, id int64, now time.Time) (provider.CardSnapshot, error) {
	reader.calls++
	if ctx.Err() != nil {
		return provider.CardSnapshot{}, ctx.Err()
	}
	return reader.snapshot, reader.err
}

type executorFunc func(context.Context, checkout.PrepaymentRequest) (checkout.PrepaymentReport, error)

func (fn executorFunc) PreparePrepayment(ctx context.Context, request checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
	return fn(ctx, request)
}

func fixtureQuote() checkout.ReviewedQuote {
	return checkout.ReviewedQuote{Plan: "plus", Country: "PH", Currency: "PHP", MinorUnitScale: 2,
		SubtotalMinor: 98214, TaxMinor: 11786, TotalMinor: 110000, BudgetMinMinor: 100000, BudgetMaxMinor: 120000}
}

func fixtureOptions() Options {
	return Options{ChromePath: "synthetic-chrome", ShowBrowser: true, Timeout: time.Minute}
}

func successfulExecutor(t *testing.T, input Input, mutate func(*checkout.PrepaymentReport)) ExecutorFactory {
	return func(Options) (checkout.PrepaymentExecutor, error) {
		return executorFunc(func(ctx context.Context, request checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
			t.Helper()
			billing, err := request.ResolveBilling(ctx)
			if err != nil {
				return checkout.PrepaymentFailure("PREPAYMENT_BILLING_UNAVAILABLE"), err
			}
			if billing.Country != "US" {
				t.Fatal("actual billing country was replaced by checkout market")
			}
			quote := fixtureQuote()
			material, err := request.ResolveMaterial(ctx, quote)
			if err != nil {
				return checkout.PrepaymentFailure("PREPAYMENT_MATERIAL_UNAVAILABLE"), err
			}
			if material.Billing != *billing {
				t.Fatal("two stages used different card billing")
			}
			if _, err := request.ResolveMaterial(ctx, quote); err != nil {
				t.Fatal(err)
			}
			report := checkout.PrepaymentReport{Operation: "prepare-payment", Code: "PREPAYMENT_REVIEW_READY", Stage: "review_ready",
				TargetFingerprint: input.fingerprint, CookieReadbackVerified: true, IdentityMatch: true, BillingVerified: true, FieldsVerified: true,
				Quote: &quote, EvidenceSource: "visible_dom", CountrySource: "requested_checkout_market", ReadyForConfirmation: true, BrowserOpened: true, SubmissionFenceVerified: true}
			if mutate != nil {
				mutate(&report)
			}
			request.OnReady(report)
			report.BrowserClosed = true
			return report, nil
		}), nil
	}
}

func TestParseIsStrictAndProtectedInputIsNotPrintable(t *testing.T) {
	input := fixtureInput(t)
	for _, value := range []any{input, Check(input)} {
		encoded, _ := json.Marshal(value)
		for _, output := range []string{string(encoded), fmt.Sprintf("%v", value), fmt.Sprintf("%#v", value)} {
			for _, secret := range []string{"synthetic-session", "synthetic-api", "fixture-owner", "synthetic-app-id"} {
				if strings.Contains(output, secret) {
					t.Fatal("protected input leaked")
				}
			}
		}
	}
	cases := map[string]string{
		"duplicate":         strings.Replace(string(inputJSON()), `"target":`, `"target":{},"TARGET":`, 1),
		"unknown-operation": strings.Replace(string(inputJSON()), `"card":`, `"pay":true,"card":`, 1),
		"credential-url":    strings.Replace(string(inputJSON()), `"kind":"zovocard"`, `"kind":"zovocard","baseURL":"https://untrusted.invalid"`, 1),
		"wrong-target":      strings.Replace(string(inputJSON()), `"plan":"plus"`, `"plan":"go"`, 1),
		"numeric-budget":    strings.Replace(string(inputJSON()), `"min":"1000.00"`, `"min":1000`, 1),
		"zero-card":         strings.Replace(string(inputJSON()), `"cardId":7`, `"cardId":0`, 1),
		"access-only":       strings.Replace(string(inputJSON()), `"sessionToken":`, `"accessToken":`, 1),
		"secret-control":    strings.Replace(string(inputJSON()), `synthetic-api-secret`, `synthetic-api-secret\n`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(raw), fixtureNow)
			if err == nil || strings.Contains(err.Error(), "synthetic") {
				t.Fatal("invalid or sensitive input accepted", err)
			}
		})
	}
	if _, err := Parse(make([]byte, MaxInputBytes+1), fixtureNow); SafeCode(err) != "PREPARATION_INPUT_TOO_LARGE" {
		t.Fatal(err)
	}
}

func TestOneSnapshotSupportsBothStagesWithoutInventingBalanceCurrency(t *testing.T) {
	input := fixtureInput(t)
	reader := &snapshotReader{snapshot: fixtureSnapshot()}
	legacyAllow := false
	reader.snapshot.GoogleChatGPTBlocked = &legacyAllow
	ready := 0
	report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, nil),
		func(config provider.CardPlatformConfig) (provider.CardSnapshotReader, error) {
			if config.Key != "zovo-fixture" || config.Kind != provider.CardPlatformZovo || config.BaseURL != "" {
				t.Fatal("card configuration changed")
			}
			return reader, nil
		}, func() time.Time { return fixtureNow }, Callbacks{OnReady: func(report Report) {
			ready++
			if report.BrowserClosed || report.PaymentAllowed || report.FundingActionsTaken != 0 || report.Card == nil {
				t.Fatal("ready phase was not prepayment")
			}
		}})
	if err != nil || reader.calls != 1 || ready != 1 || !report.ReadyForConfirmation || !report.BrowserClosed || report.PaymentAllowed || report.SubmitActivated {
		t.Fatal(report, err, reader.calls)
	}
	if report.Card.Currency != "" || report.Card.BalanceCurrencyKnown || report.Card.BalanceComparable || report.Card.BalanceSufficient != nil || report.Card.MerchantRestrictionsKnown || len(report.Unconfirmed) < 2 {
		t.Fatal("unknown balance became verified", report.Card)
	}
	encoded, _ := json.Marshal(report)
	for _, secret := range []string{"424242424242", "synthetic-api", "synthetic-session", "Fixture Owner", "Fixture Road", "fixture-owner"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("report leaked protected material")
		}
	}
}

func TestSnapshotBindingStatusMerchantAndExpiryStopBeforeMaterial(t *testing.T) {
	for name, mutate := range map[string]func(*provider.CardSnapshot){
		"different-card":     func(s *provider.CardSnapshot) { s.UpstreamCardID++ },
		"unverified-binding": func(s *provider.CardSnapshot) { s.CardBindingVerified = false },
		"inactive":           func(s *provider.CardSnapshot) { s.Material.Status = "FROZEN" },
		"expired":            func(s *provider.CardSnapshot) { s.Material.ExpiryYear = "2020" },
		"merchant-blocked":   func(s *provider.CardSnapshot) { s.RestrictedMerchants = []string{"OpenAI"} },
		"legacy-block":       func(s *provider.CardSnapshot) { blocked := true; s.GoogleChatGPTBlocked = &blocked },
	} {
		t.Run(name, func(t *testing.T) {
			input := fixtureInput(t)
			reader := &snapshotReader{snapshot: fixtureSnapshot()}
			mutate(&reader.snapshot)
			report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, nil),
				func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, func() time.Time { return fixtureNow }, Callbacks{})
			if err == nil || report.ReadyForConfirmation || reader.calls != 1 {
				t.Fatal("invalid card reached ready", report, err)
			}
		})
	}
}

func TestBillingOnlyFillsBoundGapsAndRejectsConflicts(t *testing.T) {
	input := fixtureInput(t)
	input.billing = &provider.BillingAddress{Name: "Fixture Owner", Line1: "1 Fixture Road", City: "Fixture City", Country: "US"}
	reader := &snapshotReader{snapshot: fixtureSnapshot()}
	reader.snapshot.Material.Billing = nil
	resolver := &cardResolver{input: input, factory: func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, now: func() time.Time { return fixtureNow }}
	material, err := resolver.load(context.Background())
	if err != nil || material.Billing.Country != "US" || resolver.review.BillingSource != "provider_and_bound_input" {
		t.Fatal(material, err)
	}
	conflicting := *input.billing
	conflicting.Country = "PH"
	if _, _, err := mergeBilling(*fixtureSnapshot().Material.Billing, &conflicting); SafeCode(err) != "PREPARATION_BILLING_CONFLICT" {
		t.Fatal("conflicting actual billing accepted", err)
	}
	if _, _, err := mergeBilling(provider.BillingAddress{Name: "Fixture Owner"}, nil); SafeCode(err) != "PREPARATION_BILLING_INCOMPLETE" {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(inputJSON(), &payload)
	payload["billing"] = map[string]any{"platformKey": "another-account", "cardId": 7, "address": map[string]any{"name": "Fixture Owner"}}
	raw, _ := json.Marshal(payload)
	if _, err := Parse(raw, fixtureNow); SafeCode(err) != "PREPARATION_BILLING_BINDING_MISMATCH" {
		t.Fatal(err)
	}
}

func TestOnlySameCurrencyCanProveBalanceSufficiency(t *testing.T) {
	for _, currency := range []string{"", "USD", "PHP"} {
		t.Run(currency, func(t *testing.T) {
			input := fixtureInput(t)
			reader := &snapshotReader{snapshot: fixtureSnapshot()}
			reader.snapshot.Currency = currency
			report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, nil),
				func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, func() time.Time { return fixtureNow }, Callbacks{})
			if currency == "PHP" {
				if SafeCode(err) != "PREPARATION_BALANCE_INSUFFICIENT" || report.ReadyForConfirmation {
					t.Fatal(report, err)
				}
			} else if err != nil || report.Card.BalanceSufficient != nil || report.Card.BalanceComparable {
				t.Fatal("cross-currency amount compared", report, err)
			}
		})
	}
}

func TestFinalQuoteRechecksPreviouslySufficientSameCurrencyBalance(t *testing.T) {
	input := fixtureInput(t)
	reader := &snapshotReader{snapshot: fixtureSnapshot()}
	reader.snapshot.Currency, reader.snapshot.AvailableAmount = "PHP", 1110
	ready := false
	report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, func(report *checkout.PrepaymentReport) {
		// The final tax changes, still within the user's budget, after the first
		// quote used for the initial material check. The card no longer covers it.
		report.Quote.TotalMinor, report.Quote.TaxMinor = 115000, 16786
	}), func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil },
		func() time.Time { return fixtureNow }, Callbacks{OnReady: func(Report) { ready = true }})
	if SafeCode(err) != "PREPARATION_BALANCE_INSUFFICIENT" || report.ReadyForConfirmation || ready || reader.calls != 1 {
		t.Fatal("final price reused the earlier affordability result", report, err)
	}
}

func TestReadyRequiresEveryEvidenceAndNeverAcceptsSubmission(t *testing.T) {
	mutations := map[string]func(*checkout.PrepaymentReport){
		"identity":           func(r *checkout.PrepaymentReport) { r.IdentityMatch = false },
		"readback":           func(r *checkout.PrepaymentReport) { r.CookieReadbackVerified = false },
		"billing":            func(r *checkout.PrepaymentReport) { r.BillingVerified = false },
		"fields":             func(r *checkout.PrepaymentReport) { r.FieldsVerified = false },
		"fingerprint":        func(r *checkout.PrepaymentReport) { r.TargetFingerprint = "0000000000000000" },
		"amount":             func(r *checkout.PrepaymentReport) { r.Quote.TotalMinor++ },
		"missing":            func(r *checkout.PrepaymentReport) { r.MissingFacts = []string{"tax"} },
		"payment-permission": func(r *checkout.PrepaymentReport) { r.PaymentAllowed = true },
		"submitted":          func(r *checkout.PrepaymentReport) { r.SubmitActivated = true },
		"blocked-submit":     func(r *checkout.PrepaymentReport) { r.SubmissionBlocked = true },
		"missing-fence":      func(r *checkout.PrepaymentReport) { r.SubmissionFenceVerified = false },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			input := fixtureInput(t)
			reader := &snapshotReader{snapshot: fixtureSnapshot()}
			ready := false
			report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, mutate),
				func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, func() time.Time { return fixtureNow }, Callbacks{OnReady: func(Report) { ready = true }})
			if SafeCode(err) != "PREPARATION_REPORT_INVALID" || ready || report.ReadyForConfirmation || report.PaymentAllowed {
				t.Fatal("incomplete evidence accepted", report, err)
			}
		})
	}
}

func TestCancellationAndProviderErrorsDoNotLeakOrBecomeReady(t *testing.T) {
	input := fixtureInput(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	_, err := Run(ctx, input, fixtureOptions(), func(Options) (checkout.PrepaymentExecutor, error) { called = true; return nil, nil }, NewCardReader, func() time.Time { return fixtureNow }, Callbacks{})
	if SafeCode(err) != "PREPAYMENT_CANCELLED" || called {
		t.Fatal("cancelled run started executor", err)
	}
	reader := &snapshotReader{err: errors.New("synthetic-api-secret at private endpoint")}
	report, err := Run(context.Background(), input, fixtureOptions(), successfulExecutor(t, input, nil),
		func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { return reader, nil }, func() time.Time { return fixtureNow }, Callbacks{})
	encoded, _ := json.Marshal(report)
	if SafeCode(err) != "PREPARATION_CARD_UNAVAILABLE" || strings.Contains(string(encoded)+err.Error(), "synthetic-api") || report.ReadyForConfirmation {
		t.Fatal("unsafe failure", err)
	}
}
