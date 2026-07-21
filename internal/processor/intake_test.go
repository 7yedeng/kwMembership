package processor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"kwmembership/internal/store"
)

func TestIntakeCreatesMembershipFulfillmentOnce(t *testing.T) {
	repository, err := store.Open(filepath.Join(t.TempDir(), "kawang.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	db := repository.DB()
	if _, err := db.Exec(`
    CREATE TABLE products (id TEXT PRIMARY KEY, membership_tier TEXT);
    CREATE TABLE cdkeys (id TEXT PRIMARY KEY, manual_type TEXT, metadata TEXT);
    CREATE TABLE redeem_orders (
      id TEXT PRIMARY KEY, order_no TEXT NOT NULL, cdkey_id TEXT, site_id TEXT NOT NULL,
      product_id TEXT NOT NULL, extension_delivery_status TEXT, extension_delivered_at TEXT,
      created_at TEXT NOT NULL
    );
    CREATE TABLE membership_fulfillments (
      id TEXT PRIMARY KEY, order_id TEXT NOT NULL UNIQUE, order_no TEXT NOT NULL UNIQUE,
      target_tier TEXT NOT NULL, state TEXT NOT NULL, current_stage TEXT, run_mode TEXT,
      account_lock_key TEXT, resume_revision INTEGER NOT NULL DEFAULT 0,
      state_revision INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
    );
		CREATE TABLE automatic_checkout_scopes (
		  id TEXT PRIMARY KEY, site_id TEXT NOT NULL, product_id TEXT NOT NULL, tier TEXT NOT NULL,
		  status TEXT NOT NULL, activated_at TEXT
		);
		CREATE TABLE membership_intake_settings (
		  id TEXT PRIMARY KEY, accept_orders_created_at TEXT NOT NULL,
		  created_at TEXT NOT NULL, created_by TEXT NOT NULL
		);
		CREATE TABLE admin_audit_logs (
      id TEXT PRIMARY KEY, action TEXT NOT NULL, actor TEXT NOT NULL,
      resource_type TEXT NOT NULL, resource_id TEXT, detail TEXT, created_at TEXT NOT NULL
    );
    INSERT INTO products VALUES ('product-plus', 'plus');
    INSERT INTO cdkeys VALUES ('key-1', NULL, '{}');
		INSERT INTO redeem_orders VALUES (
		  'order-1', 'KW-1', 'key-1', 'site-1', 'product-plus', 'succeeded',
		  '2026-07-20T00:01:00.000Z', '2026-07-20T00:00:00.000Z'
		);
		INSERT INTO redeem_orders VALUES (
		  'order-historical', 'KW-HISTORICAL', 'key-1', 'site-1', 'product-plus', 'succeeded',
		  '2026-07-19T00:01:00.000Z', '2026-07-19T00:00:00.000Z'
		);
		INSERT INTO membership_intake_settings VALUES (
		  'default', '2026-07-20T00:00:00.000Z', '2026-07-20T00:00:00.000Z', 'test'
		);
		INSERT INTO automatic_checkout_scopes VALUES (
      'scope-1', 'site-1', 'product-plus', 'plus', 'active', '2026-07-19T00:00:00.000Z'
    );`); err != nil {
		t.Fatal(err)
	}

	runner := &Processor{
		store: repository,
		now:   func() time.Time { return time.Date(2026, 7, 20, 0, 2, 0, 0, time.UTC) },
	}
	processed, err := runner.tickIntake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !processed {
		t.Fatal("membership order was not ingested")
	}
	var state, tier, runMode string
	if err := db.QueryRow(`
    SELECT state, target_tier, run_mode FROM membership_fulfillments WHERE order_id='order-1'
  `).Scan(&state, &tier, &runMode); err != nil {
		t.Fatal(err)
	}
	if state != "WAITING_SESSION_VALIDATION" || tier != "plus" || runMode != "automatic" {
		t.Fatalf("fulfillment = state %q tier %q runMode %q", state, tier, runMode)
	}
	var historicalCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM membership_fulfillments WHERE order_id='order-historical'").Scan(&historicalCount); err != nil {
		t.Fatal(err)
	}
	if historicalCount != 0 {
		t.Fatalf("historical fulfillment count = %d, want 0", historicalCount)
	}
	processed, err = runner.tickIntake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed {
		t.Fatal("idempotent intake created the order twice")
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM membership_fulfillments").Scan(&count); err != nil || count != 1 {
		t.Fatalf("fulfillment count = %d, error = %v", count, err)
	}
}
