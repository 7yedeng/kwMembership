package preflight

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"kwmembership/internal/checkout"
)

const syntheticEgyptGoMetadata = `{"expectedEmail":"buyer@example.invalid","targetTier":"go","priceContract":{"id":"synthetic-eg-go","version":1,"tier":"go","country":"EG","currency":"EGP","minAmount":100,"maxAmount":200}}`

func changedEgyptMetadata(t *testing.T, change func(map[string]any, map[string]any)) string {
	t.Helper()
	var item map[string]any
	if err := json.Unmarshal([]byte(syntheticEgyptGoMetadata), &item); err != nil {
		t.Fatal(err)
	}
	change(item, item["priceContract"].(map[string]any))
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEgyptMetadataRequiresAnExplicitGoEGAndEGPPair(t *testing.T) {
	prepared, err := DecodeInteractive(strings.NewReader(syntheticEgyptGoMetadata))
	if err != nil || prepared.request.PriceContract.Country != "EG" || prepared.request.PriceContract.Currency != "EGP" {
		t.Fatalf("explicit synthetic Egypt observation metadata was not retained: %v", err)
	}
	for name, change := range map[string]func(map[string]any, map[string]any){
		"country omitted": func(_ map[string]any, price map[string]any) { delete(price, "country") },
		"country empty":   func(_ map[string]any, price map[string]any) { price["country"] = "" },
		"PH with EGP":     func(_ map[string]any, price map[string]any) { price["country"] = "PH" },
		"EG with PHP":     func(_ map[string]any, price map[string]any) { price["currency"] = "PHP" },
		"other currency":  func(_ map[string]any, price map[string]any) { price["currency"] = "USD" },
		"Plus expansion": func(item map[string]any, price map[string]any) {
			item["targetTier"], price["tier"] = "plus", "plus"
		},
		"Go under Plus contract": func(_ map[string]any, price map[string]any) { price["tier"] = "plus" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeInteractive(strings.NewReader(changedEgyptMetadata(t, change))); err == nil {
				t.Fatal("unsupported market pair or plan was accepted by interactive metadata")
			}
		})
	}
}

func TestEgyptMetadataDoesNotExtendAutomaticSessionRequests(t *testing.T) {
	for _, tier := range []string{"plus", "go"} {
		t.Run(tier, func(t *testing.T) {
			raw := testInput(t, func(item map[string]any) {
				item["targetTier"] = tier
				price := item["priceContract"].(map[string]any)
				price["tier"], price["country"], price["currency"] = tier, "EG", "EGP"
			})
			if _, err := Decode(strings.NewReader(raw), testNow); err == nil {
				t.Fatal("Egypt observation parameters enabled an automatic Session checkout request")
			}
		})
	}
	// Existing Session input still omits country and retains its PH/PHP
	// compatibility behavior. The Session here is an entirely synthetic value.
	if _, err := Decode(strings.NewReader(testInput(t, nil)), testNow); err != nil {
		t.Fatalf("legacy PH/PHP Session input lost backward compatibility: %v", err)
	}
	for _, metadata := range []string{interactiveMetadata, strings.ReplaceAll(interactiveMetadata, `"plus"`, `"go"`)} {
		if _, err := DecodeInteractive(strings.NewReader(metadata)); err != nil {
			t.Fatalf("legacy PH/PHP interactive metadata lost backward compatibility: %v", err)
		}
	}
}

func TestEgyptGoObservationMatchesPlanAndMarketWithoutPayment(t *testing.T) {
	prepared, err := DecodeInteractive(strings.NewReader(syntheticEgyptGoMetadata))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name, plan, country, currency string
		passes                        bool
	}{
		{"matching", "go", "EG", "EGP", true},
		{"Philippines page", "go", "PH", "PHP", false},
		{"country mismatch", "go", "PH", "EGP", false},
		{"currency mismatch", "go", "EG", "PHP", false},
		{"missing country", "go", "", "EGP", false},
		{"missing currency", "go", "EG", "", false},
		{"Plus page", "plus", "EG", "EGP", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			report, err := RunInteractive(context.Background(), prepared, executorFunc(func(_ context.Context, request checkout.Request) (checkout.Result, error) {
				if request.Mode != checkout.ModeInteractivePreflight || request.Stage != "go" || request.TargetTier != "go" ||
					request.PriceContract.Country != "EG" || request.PriceContract.Currency != "EGP" ||
					len(request.Session) != 0 || len(request.Cookies) != 0 || request.Material != nil || request.Guard != nil ||
					request.Binding != (checkout.ExecutionBinding{}) {
					t.Fatal("Egypt observation acquired credentials, payment capabilities or another target")
				}
				page := testPage()
				page.Plan, page.Country, page.Currency = item.plan, item.country, item.currency
				// 193 is synthetic test data only. This test does not establish
				// a current Egyptian Go price, a SKU, or permission to pay.
				amount := 193.0
				page.DisplayedAmount = &amount
				return checkout.Result{Page: page}, nil
			}), func() error { return nil })
			if report.PaymentAllowed {
				t.Fatal("observation granted payment permission")
			}
			if item.passes {
				if err != nil || !report.PreflightPassed || !report.RemoteVerified || report.Page == nil ||
					report.Page.Plan != "go" || report.Page.Country != "EG" || report.Page.Currency != "EGP" || report.Page.DisplayedAmount != 193 {
					t.Fatalf("matching synthetic Egypt facts were not retained: %v", err)
				}
			} else if SafeCode(err) != "CHECKOUT_UI_UNSUPPORTED" || report.RemoteVerified || report.PreflightPassed {
				t.Fatal("wrong or unknown observed plan/market satisfied an Egypt Go request")
			}
			assertReportSanitized(t, report)
		})
	}
}
