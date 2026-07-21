package processor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/config"
	"kwmembership/internal/domain"
	"kwmembership/internal/secure"
	"kwmembership/internal/store"
)

func TestCatchUpSessionActivationMovesInvalidRowsOutOfFixedWindow(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "activation-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	if _, err := repository.DB().Exec(`CREATE TABLE membership_fulfillments (
    id TEXT PRIMARY KEY,order_id TEXT NOT NULL,order_no TEXT NOT NULL,target_tier TEXT NOT NULL,state TEXT NOT NULL,
    current_stage TEXT,run_mode TEXT,account_lock_key TEXT,state_revision INTEGER NOT NULL DEFAULT 0,retry_at TEXT,
    money_boundary_at TEXT,card_reservation_id TEXT,failure_code TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
    completed_at TEXT
  ); CREATE TABLE membership_outbox (
    id TEXT PRIMARY KEY,event_type TEXT NOT NULL,fulfillment_id TEXT,state_revision INTEGER,payload TEXT NOT NULL,created_at TEXT NOT NULL
  ); CREATE TABLE redeem_orders (
    id TEXT PRIMARY KEY,extension_delivery_status TEXT NOT NULL,session_payload TEXT NOT NULL
  )`); err != nil {
		t.Fatal(err)
	}
	at := "2026-07-20T00:00:00.000Z"
	if _, err := repository.DB().Exec(`INSERT INTO membership_fulfillments
    (id,order_id,order_no,target_tier,state,state_revision,created_at,updated_at)
    VALUES ('mf-bad','order-bad','ORDER-BAD','plus','WAITING_SESSION_ACTIVATION',0,?,?);
    INSERT INTO redeem_orders (id,extension_delivery_status,session_payload)
    VALUES ('order-bad','succeeded','not-an-encrypted-session')`, at, at); err != nil {
		t.Fatal(err)
	}
	crypt, err := secure.NewDecrypter("activation-test-secret")
	if err != nil {
		t.Fatal(err)
	}
	processor := &Processor{store: repository, decrypter: crypt, config: config.Config{EncryptionKey: "activation-test-secret-with-32-characters"}}
	changed, err := processor.catchUpSessionActivation(ctx, 20, time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var state, code string
	var retry string
	if err := repository.DB().QueryRow(`SELECT state,failure_code,retry_at FROM membership_fulfillments WHERE id='mf-bad'`).
		Scan(&state, &code, &retry); err != nil {
		t.Fatal(err)
	}
	if changed != 1 || state != "MEMBERSHIP_CONTRACT_UNKNOWN" || code != "SESSION_INVALID" || retry == "" {
		t.Fatalf("invalid activation projection changed=%d state=%s code=%s retry=%s", changed, state, code, retry)
	}
}

func TestInventoryPlanRequiresCapacityAndFreshCompletePrices(t *testing.T) {
	ctx := context.Background()
	repository, err := store.Open(filepath.Join(t.TempDir(), "eligibility.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	repository.DB().Exec(`CREATE TABLE managed_cards (
    id TEXT PRIMARY KEY,product_code TEXT,lane TEXT,consumed_slots INTEGER,capacity_state TEXT,
    upstream_status TEXT,reconciliation_state TEXT
  ); CREATE TABLE card_capacity_reservations (
    id TEXT,fulfillment_id TEXT,card_id TEXT,target_lane TEXT,state TEXT
  ); CREATE TABLE card_price_signals (
    card_id TEXT,tier TEXT,found INTEGER,amount REAL,provider_time TEXT
  ); CREATE TABLE card_product_policies (product_code TEXT,enabled INTEGER);`)
	if _, err := repository.DB().Exec(`INSERT INTO managed_cards VALUES
    ('card-1','p1','x20',0,'AVAILABLE','ACTIVE','READY');
    INSERT INTO card_price_signals VALUES
    ('card-1','plus',1,16.2,'2026-07-20T00:00:00Z'),
    ('card-1','x20',1,140.0,'2026-07-20T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	processor := &Processor{store: repository}
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	ready, err := processor.inventoryPlanAvailable(ctx, domain.TierX20, now)
	if err != nil || !ready {
		t.Fatalf("fresh plan ready=%t err=%v", ready, err)
	}
	if _, err := repository.DB().Exec(`INSERT INTO card_capacity_reservations VALUES
    ('reservation-1','mf-1','card-1','x20','reserved')`); err != nil {
		t.Fatal(err)
	}
	ready, err = processor.inventoryPlanAvailable(ctx, domain.TierX20, now)
	if err != nil {
		t.Fatal(err)
	}
	if ready {
		t.Fatal("full x20 card remained eligible")
	}
}
