package preparation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/provider"
)

func TestEntryDiagnosticSurvivesPreparationWithoutCardRequests(t *testing.T) {
	for _, reason := range []string{"account_http_rejected", "private-response-containing-secret"} {
		t.Run(reason, func(t *testing.T) {
			input := fixtureInput(t)
			reads := 0
			executor := func(Options) (checkout.PrepaymentExecutor, error) {
				return executorFunc(func(context.Context, checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
					observed := checkout.PrepaymentFailure("PREPAYMENT_ELIGIBILITY_UNCONFIRMED")
					observed.Stage = "opening_checkout"
					observed.CookieReadbackVerified, observed.IdentityMatch = true, true
					observed.BrowserOpened, observed.BrowserClosed = true, true
					observed.CheckoutDiagnostic = &checkout.CheckoutDiagnostic{Phase: "account_lookup", Reason: reason,
						Source: "none", SessionHTTPStatus: 200, AccountHTTPStatus: 403, AccountIDPresent: true,
						AccountBinding: "session", Plan: "unknown", ActiveSubscription: "unknown"}
					return observed, fail("PREPAYMENT_ELIGIBILITY_UNCONFIRMED")
				}), nil
			}
			report, err := Run(context.Background(), input, fixtureOptions(), executor,
				func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) { reads++; return nil, nil },
				func() time.Time { return fixtureNow }, Callbacks{})
			want := reason
			if strings.HasPrefix(reason, "private-") {
				want = "none"
			}
			if err == nil || report.Code != "PREPAYMENT_ELIGIBILITY_UNCONFIRMED" || report.Stage != "opening_checkout" ||
				report.CheckoutDiagnostic == nil || report.CheckoutDiagnostic.Reason != want || report.CheckoutDiagnostic.AccountHTTPStatus != 403 ||
				report.CheckoutDiagnostic.CheckoutRequested || reads != 0 || report.Card != nil || report.FundingActionsTaken != 0 || report.PaymentAllowed {
				t.Fatal("preparation lost entry facts or touched the card platform")
			}
			encoded, marshalErr := json.Marshal(report)
			if marshalErr != nil || strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "synthetic-") {
				t.Fatal("protected entry details escaped the report")
			}
		})
	}
}
