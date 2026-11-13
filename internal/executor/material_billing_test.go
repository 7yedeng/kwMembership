package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/checkout"
	"kwmembership/internal/store"
)

func TestPaymentMaterialBridgePreservesLine2AndActualBillingCountry(t *testing.T) {
	// All gate state and material below belong to a disposable local test DB.
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "billing-bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	createBridgeSchema(t, repository)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
      (id,state,run_mode,state_revision,card_reservation_id,updated_at)
      VALUES ('mf-billing','CHECKOUT_EXECUTION_WAIT','canary',3,'reservation-fixture',?)`, store.ISO(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO membership_payment_stages
      VALUES ('mf-billing','plus',1,?,'fixture-price','checkout_ready','fixture-card')`, AdapterVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.DB().Exec(`UPDATE membership_fulfillment_settings SET enabled=1,rollout_mode='canary' WHERE id='default';
      INSERT INTO card_capacity_reservations VALUES ('reservation-fixture','reserved','fixture-card')`); err != nil {
		t.Fatal(err)
	}
	bridge, err := NewBridge(repository.DB(), bridgeTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	bridge.now = func() time.Time { return now }
	server := httptest.NewServer(bridge.Handler())
	defer server.Close()
	request := checkout.Request{
		Mode: checkout.ModeSessionCheckout, Stage: "plus", TargetTier: "plus", ExpectedEmail: "fixture@example.com",
		Session:       json.RawMessage(`{"sessionToken":"synthetic-session","user":{"email":"fixture@example.com"}}`),
		PriceContract: checkout.PriceContract{ID: "fixture-price", Version: 1, Tier: "plus", Currency: "PHP", MinAmount: 1000, MaxAmount: 1200},
		Binding:       checkout.ExecutionBinding{FulfillmentID: "mf-billing", FulfillmentRevision: 3, AttemptNo: 1, PriorityClass: "normal", AdapterVersion: AdapterVersion},
		Material: &checkout.Material{
			Card:    checkout.CardMaterial{Number: "4242424242424242", CVV: "123", ExpiryMonth: "01", ExpiryYear: "2030"},
			Billing: checkout.BillingAddress{Name: "Synthetic Holder", Line1: "1 Test Street", Line2: "Suite 2", City: "Test City", Country: "GB"},
		},
	}
	if _, err := bridge.Execute(ctx, request); errorCode(err) != "EXECUTOR_PENDING" {
		t.Fatalf("payment command was not queued: %v", err)
	}
	leased := postBridge(t, server.URL+"/internal/v1/executions/lease", bridgeTestSecret, nil, map[string]any{"executorId": "synthetic-python"})
	if leased.Code != http.StatusOK {
		t.Fatalf("lease status = %d", leased.Code)
	}
	var command map[string]any
	if err := json.Unmarshal(leased.Body.Bytes(), &command); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{
		"X-Executor-ID": "synthetic-python", "X-Lease-Epoch": jsonNumber(command["leaseEpoch"]), "X-Lease-Token": command["leaseToken"].(string),
	}
	response := postBridge(t, server.URL+"/internal/v1/executions/"+command["executionId"].(string)+"/material", bridgeTestSecret, headers, map[string]any{})
	if response.Code != http.StatusOK {
		t.Fatalf("material status = %d", response.Code)
	}
	var payload struct {
		Billing          checkout.BillingAddress `json:"billing"`
		CheckoutContract struct {
			Country  string `json:"country"`
			Currency string `json:"currency"`
		} `json:"checkoutContract"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Billing != request.Material.Billing {
		t.Fatal("bridge lost or replaced billing fields")
	}
	if payload.CheckoutContract.Country != "PH" || payload.CheckoutContract.Currency != "PHP" {
		t.Fatal("billing country changed the independent price contract")
	}
}
