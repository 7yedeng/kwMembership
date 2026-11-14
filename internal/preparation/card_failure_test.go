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

func TestCardFailureClassificationSurvivesRunWithoutLeakingDetails(t *testing.T) {
	const privateDetail = "private-api-secret-at-https://private-endpoint.invalid/?token=canary"
	providerFailure := func(code string) error {
		return &provider.Error{ErrorCode: code, Message: privateDetail}
	}
	var nilProviderFailure *provider.Error
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"authentication", providerFailure("ZOVOCARD_AUTH_FAILED"), "PREPARATION_CARD_AUTH_FAILED"},
		{"access-denied", providerFailure("ZOVOCARD_ACCESS_DENIED"), "PREPARATION_CARD_ACCESS_DENIED"},
		{"rate-limited", providerFailure("ZOVOCARD_RATE_LIMITED"), "PREPARATION_CARD_RATE_LIMITED"},
		{"timeout", providerFailure("ZOVOCARD_TIMEOUT"), "PREPARATION_CARD_TIMEOUT"},
		{"operation-rejected", providerFailure("ZOVOCARD_OPERATION_REJECTED"), "PREPARATION_CARD_OPERATION_REJECTED"},
		{"network", providerFailure("ZOVOCARD_UNAVAILABLE"), "PREPARATION_CARD_UNAVAILABLE"},
		{"unknown-code", providerFailure(privateDetail), "PREPARATION_CARD_UNAVAILABLE"},
		{"untyped-message", errors.New("ZOVOCARD_AUTH_FAILED " + privateDetail), "PREPARATION_CARD_UNAVAILABLE"},
		{"wrapped-code", fmt.Errorf("%s: %w", privateDetail, providerFailure("ZOVOCARD_ACCESS_DENIED")), "PREPARATION_CARD_ACCESS_DENIED"},
		{"nil-provider-error", nilProviderFailure, "PREPARATION_CARD_UNAVAILABLE"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			input := fixtureInput(t)
			reader := &snapshotReader{snapshot: fixtureSnapshot(), err: test.err}
			factories, ready, progress := 0, false, ""
			executor := func(Options) (checkout.PrepaymentExecutor, error) {
				return executorFunc(func(ctx context.Context, request checkout.PrepaymentRequest) (checkout.PrepaymentReport, error) {
					request.OnProgress("resolving_billing")
					for attempt := 0; attempt < 2; attempt++ {
						billing, err := request.ResolveBilling(ctx)
						if billing != nil || SafeCode(err) != test.want {
							t.Fatal("card failure did not stop billing with its safe classification")
						}
					}
					// Match checkout's generic billing error. Run must retain the
					// resolver's more useful safe classification and the stop stage.
					report := checkout.PrepaymentFailure("PREPAYMENT_BILLING_UNAVAILABLE")
					report.Stage = "resolving_billing"
					report.BrowserOpened, report.BrowserClosed = true, true
					report.CookieReadbackVerified, report.IdentityMatch = true, true
					return report, errors.New("PREPAYMENT_BILLING_UNAVAILABLE")
				}), nil
			}
			report, err := Run(context.Background(), input, fixtureOptions(), executor,
				func(provider.CardPlatformConfig) (provider.CardSnapshotReader, error) {
					factories++
					return reader, nil
				}, func() time.Time { return fixtureNow }, Callbacks{
					OnProgress: func(stage string) { progress = stage },
					OnReady:    func(Report) { ready = true },
				})
			if err == nil || SafeCode(err) != test.want || report.Code != test.want || report.Stage != "resolving_billing" || progress != report.Stage {
				t.Fatal("final failure lost its stage or classification")
			}
			if factories != 1 || reader.calls != 1 || ready || report.ReadyForConfirmation || report.Card != nil || report.FieldsVerified || report.PaymentAllowed || report.SubmitActivated || report.FundingActionsTaken != 0 {
				t.Fatal("failed card read retried, exposed material or advanced payment")
			}
			encoded, marshalErr := json.Marshal(report)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, output := range []string{string(encoded), fmt.Sprintf("%v %#v", err, err), fmt.Sprintf("%v %#v", report, report)} {
				for _, forbidden := range []string{privateDetail, "synthetic-api-secret", "synthetic-session", "424242424242"} {
					if strings.Contains(output, forbidden) {
						t.Fatal("protected provider details leaked into the result")
					}
				}
			}
		})
	}
}
