package executor

import (
	"context"
	"encoding/json"
	"testing"

	"kwmembership/internal/checkout"
)

func TestBridgeRejectsGoTargetOrContractBeforeQueueAccess(t *testing.T) {
	for _, mode := range []checkout.Mode{checkout.ModeSessionPreflight, checkout.ModeSessionCheckout} {
		for _, mutate := range []func(*checkout.Request){
			func(r *checkout.Request) { r.TargetTier, r.Stage, r.PriceContract.Tier = "go", "go", "go" },
			func(r *checkout.Request) { r.PriceContract.Tier = "go" },
			func(r *checkout.Request) { r.Stage, r.TargetTier, r.PriceContract.Tier = "upgrade", "x5", "go" },
		} {
			request := checkout.Request{
				Mode: mode, Stage: "plus", TargetTier: "plus", ExpectedEmail: "buyer@example.invalid",
				Session:       json.RawMessage(`{"sessionToken":"synthetic-token"}`),
				Binding:       checkout.ExecutionBinding{FulfillmentID: "synthetic-order", AdapterVersion: AdapterVersion},
				PriceContract: checkout.PriceContract{ID: "synthetic-price", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 100, MaxAmount: 200},
			}
			mutate(&request)
			// An uninitialized bridge proves unsupported plans are rejected
			// before any database queue access or pending credential retention.
			var bridge Bridge
			if _, err := bridge.Execute(context.Background(), request); errorCode(err) != "CHECKOUT_REQUEST_INVALID" {
				t.Fatalf("Go request entered the production bridge: %v", err)
			}
		}
	}
}
