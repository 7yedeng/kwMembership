package preflight

import (
	"context"
	"strings"
	"testing"

	"kwmembership/internal/checkout"
)

func TestGoInteractivePreflightMatchesTargetWithoutPaymentCapabilities(t *testing.T) {
	metadata := strings.ReplaceAll(interactiveMetadata, `"plus"`, `"go"`)
	prepared, err := DecodeInteractive(strings.NewReader(metadata))
	if err != nil {
		t.Fatal(err)
	}
	for _, observedPlan := range []string{"go", "plus", "pro", ""} {
		t.Run("observed_"+observedPlan, func(t *testing.T) {
			report, err := RunInteractive(context.Background(), prepared, executorFunc(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
				if request.Mode != checkout.ModeInteractivePreflight || request.Stage != "go" || request.TargetTier != "go" || request.PriceContract.Tier != "go" ||
					len(request.Session) != 0 || len(request.Cookies) != 0 || request.Material != nil || request.Guard != nil || request.Binding != (checkout.ExecutionBinding{}) {
					t.Fatal("Go observation acquired another target or payment capabilities")
				}
				page := testPage()
				page.Plan = observedPlan
				return checkout.Result{Page: page}, nil
			}), func() error { return nil })
			if observedPlan == "go" {
				if err != nil || !report.PreflightPassed || !report.RemoteVerified || report.Page.Plan != "go" || report.PaymentAllowed {
					t.Fatalf("Go observation was not retained safely: report=%+v err=%v", report, err)
				}
			} else if SafeCode(err) != "CHECKOUT_UI_UNSUPPORTED" || report.RemoteVerified || report.PaymentAllowed {
				t.Fatal("wrong or unknown observed plan satisfied a Go request")
			}
		})
	}
	if _, err := Run(context.Background(), prepared, nil); SafeCode(err) != "PREFLIGHT_INPUT_INVALID" {
		t.Fatal("automatic Session entry accepted interactive Go")
	}
}

func TestGoCannotUsePlusContractOrAutomaticSessionRequest(t *testing.T) {
	wrongContract := strings.Replace(interactiveMetadata, `"targetTier":"plus"`, `"targetTier":"go"`, 1)
	if _, err := DecodeInteractive(strings.NewReader(wrongContract)); SafeCode(err) != "PREFLIGHT_PRICE_CONTRACT_INVALID" {
		t.Fatal("Go accepted a Plus price contract")
	}
	automaticGo := testInput(t, func(v map[string]any) {
		v["targetTier"] = "go"
		v["priceContract"].(map[string]any)["tier"] = "go"
		// Reject the unsupported plan before processing any credentials.
		v["session"] = nil
	})
	if _, err := Decode(strings.NewReader(automaticGo), testNow); SafeCode(err) != "PREFLIGHT_TARGET_UNSUPPORTED" {
		t.Fatal("Session path attempted an unverified Go API contract")
	}
}
