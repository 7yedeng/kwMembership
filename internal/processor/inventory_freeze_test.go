package processor

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/domain"
	"kwmembership/internal/provider"
	"kwmembership/internal/store"
)

func TestFreezeCapacityFullCardAfterReconciliation(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "inventory-freeze.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "inventory-freeze", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	httpClient := &http.Client{Transport: inventoryRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Method != http.MethodPost || request.URL.Path != "/openapi/v1/cards/freeze" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		if string(body) != `{"card_id":18171,"freeze":true}` {
			t.Fatalf("body = %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"ok"}`))}, nil
	})}
	client, err := provider.NewSpaceXClient(httpClient, "app", "secret")
	if err != nil {
		t.Fatal(err)
	}
	processor := &Processor{
		config: config.Config{MaintenancePath: filepath.Join(t.TempDir(), "standby")},
		store:  repository, lease: lease, now: func() time.Time { return now }, requireLeaseFence: true,
	}
	status, err := processor.freezeCardAfterReconciliation(ctx, client, 18171, "ACTIVE", domain.HistoricalFulfillmentResult{State: domain.HistoricalStateCapacityFull})
	if err != nil {
		t.Fatal(err)
	}
	if status != "FROZEN" || calls != 1 {
		t.Fatalf("status=%s calls=%d", status, calls)
	}
}

func TestFreezeCardAfterReconciliationSkipsCardsWithoutAvailableCapacityDecision(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus string
		state          domain.HistoricalFulfillmentState
	}{
		{name: "available card", upstreamStatus: "ACTIVE", state: domain.HistoricalStateAvailable},
		{name: "reconciliation hold", upstreamStatus: "ACTIVE", state: domain.HistoricalStateReconciliationHold},
		{name: "already frozen", upstreamStatus: "FROZEN", state: domain.HistoricalStateCapacityFull},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			processor := &Processor{}
			status, err := processor.freezeCardAfterReconciliation(context.Background(), nil, 18171, test.upstreamStatus, domain.HistoricalFulfillmentResult{State: test.state})
			if err != nil || status != test.upstreamStatus {
				t.Fatalf("status=%s err=%v", status, err)
			}
		})
	}
}

func TestTargetedInventorySchedulesExistingActiveCapacityFullCard(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "inventory-freeze-schedule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillment_settings (
    id TEXT PRIMARY KEY,inventory_status TEXT NOT NULL,last_inventory_error TEXT,updated_at TEXT,updated_by TEXT
  ); CREATE TABLE card_inventory_runs (
    id TEXT PRIMARY KEY,mode TEXT NOT NULL,status TEXT NOT NULL,next_page INTEGER NOT NULL,total_cards INTEGER,
    discovered_cards INTEGER NOT NULL,processed_cards INTEGER NOT NULL,held_cards INTEGER NOT NULL,
    started_at TEXT NOT NULL,updated_at TEXT NOT NULL
  ); CREATE TABLE card_inventory_run_items (
    run_id TEXT NOT NULL,upstream_card_id INTEGER NOT NULL,status TEXT NOT NULL,attempt_count INTEGER NOT NULL,
    updated_at TEXT NOT NULL,PRIMARY KEY(run_id,upstream_card_id)
  ); CREATE TABLE managed_cards (
    upstream_card_id INTEGER PRIMARY KEY,upstream_status TEXT NOT NULL,capacity_state TEXT NOT NULL,
    reconciliation_state TEXT NOT NULL,reconciliation_reason TEXT,updated_at TEXT NOT NULL
  ); INSERT INTO membership_fulfillment_settings (id,inventory_status,updated_at)
    VALUES ('default','completed','2026-07-21T12:00:00.000Z');
  INSERT INTO managed_cards (upstream_card_id,upstream_status,capacity_state,reconciliation_state,updated_at)
    VALUES (18171,'ACTIVE','CAPACITY_FULL','READY','2026-07-21T12:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 12, 1, 0, 0, time.UTC)
	if err := repository.EnsureLeaseTable(ctx, now); err != nil {
		t.Fatal(err)
	}
	lease, err := repository.AcquireLease(ctx, "go", "inventory-freeze-schedule", "test", now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	processor := &Processor{store: repository, lease: lease, now: func() time.Time { return now }, requireLeaseFence: true}
	scheduled, err := processor.scheduleTargetedInventory(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCardID int64
	if err := repository.DB().QueryRow(`SELECT upstream_card_id FROM card_inventory_run_items`).Scan(&upstreamCardID); err != nil {
		t.Fatal(err)
	}
	if !scheduled || upstreamCardID != 18171 {
		t.Fatalf("scheduled=%v upstreamCardID=%d", scheduled, upstreamCardID)
	}
}
